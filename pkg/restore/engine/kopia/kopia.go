package kopia

import (
	"context"
	"fmt"
	"strings"

	ctlbatchv1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/batch/v1"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	backupcommon "github.com/harvester/harvester/pkg/backup/common"
	restorecommon "github.com/harvester/harvester/pkg/restore/common"
	"github.com/harvester/harvester/pkg/restore/engine"
	"github.com/harvester/harvester/pkg/restore/pvchelper"
	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
	"github.com/harvester/harvester/pkg/util/jobprogress"
	kopiautil "github.com/harvester/harvester/pkg/util/kopia"
)

const (
	jobNamePrefix = "kopia-restore"
)

type KopiaRestoreEngine struct {
	vmbo         backupcommon.VMBackupOperator
	vmro         restorecommon.VMRestoreOperator
	pvcCache     ctlcorev1.PersistentVolumeClaimCache
	pvcClient    ctlcorev1.PersistentVolumeClaimClient
	secretCache  ctlcorev1.SecretCache
	secretClient ctlcorev1.SecretClient
	jobCache     ctlbatchv1.JobCache
	jobClient    ctlbatchv1.JobClient
	clientset    kubernetes.Interface
}

func GetRestoreEngine(
	vmbo backupcommon.VMBackupOperator,
	vmro restorecommon.VMRestoreOperator,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	pvcClient ctlcorev1.PersistentVolumeClaimClient,
	secretCache ctlcorev1.SecretCache,
	secretClient ctlcorev1.SecretClient,
	jobCache ctlbatchv1.JobCache,
	jobClient ctlbatchv1.JobClient,
	clientset kubernetes.Interface,
) engine.RestoreEngine {
	return &KopiaRestoreEngine{
		vmbo:         vmbo,
		vmro:         vmro,
		pvcCache:     pvcCache,
		pvcClient:    pvcClient,
		secretCache:  secretCache,
		secretClient: secretClient,
		jobCache:     jobCache,
		jobClient:    jobClient,
		clientset:    clientset,
	}
}

func (ke *KopiaRestoreEngine) Reconcile(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	volIndex int,
) error {
	vr := ke.vmro.GetVolRestore(vmr, volIndex)
	if vr == nil {
		return fmt.Errorf("volume restore at index %d not found", volIndex)
	}
	vb := ke.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return fmt.Errorf("volume backup at index %d not found", volIndex)
	}

	namespace := ke.vmro.GetNamespace(vmr)
	pvcName := ke.vmro.GetVolRestorePVCName(vr)
	pvc, err := ke.pvcCache.Get(namespace, pvcName)
	if apierrors.IsNotFound(err) {
		return ke.createPVC(vmr, vr, vb)
	}
	if err != nil {
		return err
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return engine.ErrRetryLater
	}

	jobName := ke.jobName(vmr, vr)
	job, err := ke.jobCache.Get(namespace, jobName)
	if apierrors.IsNotFound(err) {
		return ke.createRestoreJob(vmr, vmb, vr, vb, jobName)
	}
	if err != nil {
		return err
	}
	return ke.syncFromJob(vr, job)
}

func (ke *KopiaRestoreEngine) UpdateProgress(vr *harvesterv1.VolumeRestore) (int64, error) {
	current := ke.vmro.GetVolRestoreProgress(vr)
	if current == 100 {
		return 100, nil
	}
	jobName, namespace, ok := ke.jobLocator(vr)
	if !ok {
		return int64(current), nil
	}
	pct, err := jobprogress.FetchProgressPercent(context.Background(), ke.clientset, namespace, jobName)
	if err != nil {
		return int64(current), nil
	}
	if err := ke.vmro.SetVolRestoreProgress(vr, pct); err != nil {
		return 0, err
	}
	return int64(pct), nil
}

func (ke *KopiaRestoreEngine) Delete(vmr *harvesterv1.VirtualMachineRestore, volIndex int) error {
	vr := ke.vmro.GetVolRestore(vmr, volIndex)
	if vr == nil {
		return nil
	}
	err := ke.jobClient.Delete(ke.vmro.GetNamespace(vmr), ke.jobName(vmr, vr), &metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// RegisterWatchers is a no-op for now. Once Kopia adopts the same
// label-and-watch pattern as the restic engine, it can install a Job
// watcher here to eliminate poll-based requeues.
func (ke *KopiaRestoreEngine) RegisterWatchers(_ context.Context, _ func(string, string)) {}

func (ke *KopiaRestoreEngine) createPVC(
	vmr *harvesterv1.VirtualMachineRestore,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
) error {
	pvcName := ke.vmro.GetVolRestorePVCName(vr)
	namespace := ke.vmro.GetNamespace(vmr)
	pvcSpec := ke.vmbo.GetVolBackupPVCSpec(vb)
	annotations := pvchelper.BuildRestoreAnnotations(
		ke.vmbo.GetVolBackupPVCAnnotations(vb),
		ke.vmro.GetName(vmr),
		restorecommon.RestoreNameAnnotation,
	)
	labels := pvchelper.BuildRestoreLabels(ke.vmbo.GetVolBackupPVCLabels(vb))
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      pvcSpec.AccessModes,
			Resources:        pvcSpec.Resources,
			StorageClassName: pvcSpec.StorageClassName,
			VolumeMode:       pvcSpec.VolumeMode,
		},
	}
	pvc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: harvesterv1.SchemeGroupVersion.String(),
		Kind:       "VirtualMachineRestore",
		Name:       ke.vmro.GetName(vmr),
		UID:        ke.vmro.GetUID(vmr),
	}}
	pvc.Spec.VolumeName = ""
	pvc.Spec.DataSource = nil
	pvc.Spec.DataSourceRef = nil

	_, err := ke.pvcClient.Create(pvc)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

func (ke *KopiaRestoreEngine) createRestoreJob(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
	jobName string,
) error {
	target, err := settings.DecodeBackupTarget(settings.BackupTargetSet.Get())
	if err != nil {
		return err
	}
	repository, err := kopiautil.S3Repository(target)
	if err != nil {
		return err
	}
	secretName, err := ke.ensureKopiaSecret(vmr)
	if err != nil {
		return err
	}
	image, err := kopiautil.Image()
	if err != nil {
		return err
	}
	vbName := ke.vmbo.GetVolBackupName(vb)
	if vbName == nil {
		return fmt.Errorf("volume backup name is nil")
	}

	namespace := ke.vmro.GetNamespace(vmr)
	pvcName := ke.vmro.GetVolRestorePVCName(vr)
	pvName := ke.vmbo.GetVolBackupPVName(vb)
	command := fmt.Sprintf(
		strings.Join([]string{
			"set -e",
			"%s",
			// `kopia snapshot list --json` returns an array of snapshot manifests
			// sorted oldest-first; `.[-1].rootEntry.obj` gives the object ID of
			// the newest matching snapshot. jq is far less brittle than the prior
			// sed/grep pipeline that depended on the literal output formatting.
			"OBJ=$(kopia --config-file=%s snapshot list --all --json --tags %s,%s /%s | jq -r '.[-1].rootEntry.obj')",
			"test -n \"$OBJ\" && test \"$OBJ\" != \"null\"",
			"TAR_FIFO=/tmp/kopia-restore.tar",
			"DATA_FIFO=/tmp/kopia-restore.data",
			"rm -f \"$TAR_FIFO\" \"$DATA_FIFO\"",
			"mkfifo \"$TAR_FIFO\" \"$DATA_FIFO\"",
			"trap 'rm -f \"$TAR_FIFO\" \"$DATA_FIFO\"' EXIT",
			"/usr/bin/harvester io-mode -device /dev/%s -mode=write < \"$DATA_FIFO\" &",
			"WRITER_PID=$!",
			"tar -xOf \"$TAR_FIFO\" \"/%s\" > \"$DATA_FIFO\" &",
			"TAR_PID=$!",
			"set +e",
			"kopia --config-file=%s snapshot restore --mode=tar \"$OBJ\" \"$TAR_FIFO\"",
			"KOPIA_STATUS=$?",
			"if [ \"$KOPIA_STATUS\" -ne 0 ]; then kill \"$TAR_PID\" \"$WRITER_PID\" 2>/dev/null; fi",
			"wait \"$TAR_PID\"",
			"TAR_STATUS=$?",
			"if [ \"$TAR_STATUS\" -ne 0 ]; then kill \"$WRITER_PID\" 2>/dev/null; fi",
			"wait \"$WRITER_PID\"",
			"WRITER_STATUS=$?",
			"set -e",
			"test \"$KOPIA_STATUS\" -eq 0",
			"test \"$TAR_STATUS\" -eq 0",
			"test \"$WRITER_STATUS\" -eq 0",
		}, "\n"),
		kopiautil.ConnectCommand(),
		kopiautil.ConfigPath,
		kopiautil.NamespaceTag(ke.vmbo.GetNamespace(vmb)),
		kopiautil.SnapshotTag(*vbName),
		pvName,
		pvcName,
		pvName,
		kopiautil.ConfigPath,
	)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: harvesterv1.SchemeGroupVersion.String(),
				Kind:       "VirtualMachineRestore",
				Name:       ke.vmro.GetName(vmr),
				UID:        ke.vmro.GetUID(vmr),
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](300),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "restore",
						Image:   image,
						Env:     kopiautil.Env(secretName, repository),
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{command},
						VolumeDevices: []corev1.VolumeDevice{{
							Name:       "volume",
							DevicePath: "/dev/" + pvcName,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "volume",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
						},
					}},
				},
			},
		},
	}
	_, err = ke.jobClient.Create(job)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

func (ke *KopiaRestoreEngine) ensureKopiaSecret(vmr *harvesterv1.VirtualMachineRestore) (string, error) {
	source, err := ke.secretCache.Get(util.LonghornSystemNamespaceName, util.BackupTargetSecretName)
	if err != nil {
		return "", err
	}
	if _, ok := source.Data[kopiautil.PasswordKey]; !ok {
		return "", fmt.Errorf("backup target secret %s/%s is missing %s", source.Namespace, source.Name, kopiautil.PasswordKey)
	}

	namespace := ke.vmro.GetNamespace(vmr)
	name := strings.ToLower(fmt.Sprintf("kopia-credentials-%s", ke.vmro.GetName(vmr)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: harvesterv1.SchemeGroupVersion.String(),
				Kind:       "VirtualMachineRestore",
				Name:       ke.vmro.GetName(vmr),
				UID:        ke.vmro.GetUID(vmr),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			util.AWSAccessKey:     source.Data[util.AWSAccessKey],
			util.AWSSecretKey:     source.Data[util.AWSSecretKey],
			kopiautil.PasswordKey: source.Data[kopiautil.PasswordKey],
		},
	}
	if _, err := ke.secretClient.Create(secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return name, nil
}

func (ke *KopiaRestoreEngine) syncFromJob(vr *harvesterv1.VolumeRestore, job *batchv1.Job) error {
	if job.Status.Failed > 0 {
		return fmt.Errorf("kopia restore job %s/%s failed", job.Namespace, job.Name)
	}
	if job.Status.Succeeded == 0 {
		if pct, err := jobprogress.FetchProgressPercent(context.Background(), ke.clientset, job.Namespace, job.Name); err == nil {
			_ = ke.vmro.SetVolRestoreProgress(vr, pct)
		}
		return engine.ErrRetryLater
	}
	if err := ke.vmro.SetVolRestoreProgress(vr, 100); err != nil {
		return err
	}
	logrus.Debugf("kopia restore job %s/%s completed", job.Namespace, job.Name)
	return nil
}

func (ke *KopiaRestoreEngine) jobName(vmr *harvesterv1.VirtualMachineRestore, vr *harvesterv1.VolumeRestore) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", jobNamePrefix, ke.vmro.GetName(vmr), ke.vmro.GetVolRestoreVolumeName(vr)))
}

// jobLocator returns the name and namespace to look up the restore Job for vr.
// UpdateProgress is called with only a VolumeRestore handle but the job name
// embeds the VMRestore name; we recover that from the destination PVC's
// restore-name annotation (set in createPVC via BuildRestoreAnnotations).
func (ke *KopiaRestoreEngine) jobLocator(vr *harvesterv1.VolumeRestore) (name, namespace string, ok bool) {
	volName := ke.vmro.GetVolRestoreVolumeName(vr)
	ns := ke.vmro.GetVolRestorePVCNamespace(vr)
	if volName == "" || ns == "" {
		return "", "", false
	}
	pvc, err := ke.pvcCache.Get(ns, ke.vmro.GetVolRestorePVCName(vr))
	if err != nil {
		return "", "", false
	}
	restoreName := pvc.Annotations[restorecommon.RestoreNameAnnotation]
	if restoreName == "" {
		return "", "", false
	}
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", jobNamePrefix, restoreName, volName)), ns, true
}
