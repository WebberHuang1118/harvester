package kopia

import (
	"context"
	"fmt"
	"strings"

	ctlbatchv1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/batch/v1"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/storage/v1"
	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	backupcommon "github.com/harvester/harvester/pkg/backup/common"
	restorecommon "github.com/harvester/harvester/pkg/restore/common"
	"github.com/harvester/harvester/pkg/restore/engine"
	"github.com/harvester/harvester/pkg/restore/pvchelper"
	"github.com/harvester/harvester/pkg/util"
	"github.com/harvester/harvester/pkg/util/jobprogress"
	kopiautil "github.com/harvester/harvester/pkg/util/kopia"
)

const (
	jobNamePrefix      = "kopia-restore"
	checkJobNamePrefix = "kopia-restore-check"
)

type KopiaRestoreEngine struct {
	vmbo          backupcommon.VMBackupOperator
	vmro          restorecommon.VMRestoreOperator
	pvcCache      ctlcorev1.PersistentVolumeClaimCache
	pvcClient     ctlcorev1.PersistentVolumeClaimClient
	scCache       ctlstoragev1.StorageClassCache
	secretCache   ctlcorev1.SecretCache
	secretClient  ctlcorev1.SecretClient
	jobController ctlbatchv1.JobController
	jobCache      ctlbatchv1.JobCache
	jobClient     ctlbatchv1.JobClient
	clientset     kubernetes.Interface
}

func GetRestoreEngine(
	vmbo backupcommon.VMBackupOperator,
	vmro restorecommon.VMRestoreOperator,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	pvcClient ctlcorev1.PersistentVolumeClaimClient,
	scCache ctlstoragev1.StorageClassCache,
	secretCache ctlcorev1.SecretCache,
	secretClient ctlcorev1.SecretClient,
	jobController ctlbatchv1.JobController,
	clientset kubernetes.Interface,
) engine.RestoreEngine {
	return &KopiaRestoreEngine{
		vmbo:          vmbo,
		vmro:          vmro,
		pvcCache:      pvcCache,
		pvcClient:     pvcClient,
		scCache:       scCache,
		secretCache:   secretCache,
		secretClient:  secretClient,
		jobController: jobController,
		jobCache:      jobController.Cache(),
		jobClient:     jobController,
		clientset:     clientset,
	}
}

// vmRestoreLabels returns the labels stamped on Jobs we create so the Job
// watcher in RegisterWatchers can map a Job event back to its VMRestore.
func vmRestoreLabels(vmr *harvesterv1.VirtualMachineRestore, vmro restorecommon.VMRestoreOperator) map[string]string {
	return kopiautil.JobLabels(map[string]string{
		kopiautil.LabelVMRestoreNamespace: vmro.GetNamespace(vmr),
		kopiautil.LabelVMRestoreName:      vmro.GetName(vmr),
	})
}

// RegisterWatchers registers a Job event handler that maps Job changes back to
// the owning VMRestore (via labels we stamp on the Job) and enqueues it.
func (ke *KopiaRestoreEngine) RegisterWatchers(ctx context.Context, enqueue func(namespace, name string)) {
	ke.jobController.OnChange(ctx, "kopia-restore-job-watcher", func(_ string, job *batchv1.Job) (*batchv1.Job, error) {
		if job == nil {
			return nil, nil
		}
		ns := job.Labels[kopiautil.LabelVMRestoreNamespace]
		name := job.Labels[kopiautil.LabelVMRestoreName]
		if ns != "" && name != "" {
			enqueue(ns, name)
		}
		return job, nil
	})
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
	// Once a volume has finished restoring (progress=100), bail out before any
	// Job lookup. The job watcher fires on Job deletion too, so syncFromJob's
	// post-success delete would otherwise trigger a follow-up Reconcile where
	// jobCache.Get returns NotFound and we'd spuriously recreate the Job.
	if ke.vmro.GetVolRestoreProgress(vr) == 100 {
		return nil
	}
	vb := ke.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return fmt.Errorf("volume backup at index %d not found", volIndex)
	}

	if err := ke.checkRemoteSnapshot(vmr, vmb, vr, vb); err != nil {
		return err
	}
	if err := ke.ensurePVC(vmr, vr, vb); err != nil {
		return err
	}

	jobName := ke.jobName(vmr, vr)
	namespace := ke.vmro.GetNamespace(vmr)
	job, err := ke.jobCache.Get(namespace, jobName)
	if err == nil {
		return ke.syncFromJob(vr, job)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := ke.createRestoreJob(vmr, vmb, vr, vb, jobName); err != nil {
		return err
	}
	return engine.ErrRetryLater
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

func (ke *KopiaRestoreEngine) ensurePVC(
	vmr *harvesterv1.VirtualMachineRestore,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
) error {
	namespace := ke.vmro.GetNamespace(vmr)
	pvcName := ke.vmro.GetVolRestorePVCName(vr)
	pvc, err := ke.pvcCache.Get(namespace, pvcName)
	if err == nil {
		// WaitForFirstConsumer storage classes only bind once a pod (the restore Job)
		// mounts the PVC, so gating here would deadlock that path.
		if ke.waitsForImmediateBinding(pvc) && pvc.Status.Phase != corev1.ClaimBound {
			return engine.ErrRetryLater
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	pvcSpec := ke.vmbo.GetVolBackupPVCSpec(vb)
	annotations := pvchelper.BuildRestoreAnnotations(
		ke.vmbo.GetVolBackupPVCAnnotations(vb),
		ke.vmro.GetName(vmr),
		restorecommon.RestoreNameAnnotation,
	)
	labels := pvchelper.BuildRestoreLabels(ke.vmbo.GetVolBackupPVCLabels(vb))
	pvc = &corev1.PersistentVolumeClaim{
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

	_, err = ke.pvcClient.Create(pvc)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

func (ke *KopiaRestoreEngine) waitsForImmediateBinding(pvc *corev1.PersistentVolumeClaim) bool {
	scName := ptr.Deref(pvc.Spec.StorageClassName, "")
	if scName == "" {
		return true
	}
	sc, err := ke.scCache.Get(scName)
	if err != nil || sc.VolumeBindingMode == nil {
		return true
	}
	return *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer
}

func (ke *KopiaRestoreEngine) createRestoreJob(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
	jobName string,
) error {
	repository, err := kopiautil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := ke.ensureKopiaSecret(vmr)
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
	labels := vmRestoreLabels(vmr, ke.vmro)
	runtime, hasCapacity, err := kopiautil.NewJobRuntime(context.Background(), ke.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}
	tags := ke.snapshotTags(vmb, vb)
	command := fmt.Sprintf(
		strings.Join([]string{
			"set -eo pipefail",
			// `kopia snapshot list --json` returns an array of snapshot manifests
			// sorted oldest-first; `.[-1].rootEntry.obj` gives the object ID of
			// the newest matching snapshot. jq is far less brittle than the prior
			// sed/grep pipeline that depended on the literal output formatting.
			"%s",
			"%s show \"$OBJ/%s\" | /usr/bin/harvester io-mode -device /dev/%s -mode=write",
		}, "\n"),
		kopiautil.SnapshotObjectCommand(kopiautil.ConnectCommand(), tags),
		runtime.Command,
		pvName,
		pvcName,
	)
	volumes := runtime.VolumesWith(corev1.Volume{
		Name: "volume",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
		},
	})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels:    labels,
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
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:         "restore",
						Image:        runtime.Image,
						Env:          runtime.Env,
						Command:      []string{"/bin/sh", "-c"},
						Resources:    runtime.Resources,
						VolumeMounts: runtime.VolumeMounts,
						Args:         []string{command},
						VolumeDevices: []corev1.VolumeDevice{{
							Name:       "volume",
							DevicePath: "/dev/" + pvcName,
						}},
					}},
					Volumes: volumes,
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

func (ke *KopiaRestoreEngine) checkRemoteSnapshot(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
) error {
	checkName := ke.checkJobName(vmr, vr)
	namespace := ke.vmro.GetNamespace(vmr)
	result, err := kopiautil.CheckSnapshotJob(
		context.Background(),
		ke.clientset,
		checkName,
		func(name string) (*batchv1.Job, error) {
			return ke.jobCache.Get(namespace, name)
		},
		func(name string) error {
			return ke.createCheckJob(vmr, vmb, vb, name)
		},
	)
	if err != nil {
		return err
	}
	switch result {
	case kopiautil.SnapshotCheckPending:
		return engine.ErrRetryLater
	case kopiautil.SnapshotCheckMissing:
		return fmt.Errorf("kopia snapshot for restore job %s/%s not found", namespace, checkName)
	case kopiautil.SnapshotCheckFailed:
		return fmt.Errorf("kopia snapshot check job %s/%s failed", namespace, checkName)
	case kopiautil.SnapshotCheckFound:
		return nil
	default:
		return fmt.Errorf("unknown kopia snapshot check result %q", result)
	}
}

func (ke *KopiaRestoreEngine) createCheckJob(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	jobName string,
) error {
	repository, err := kopiautil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := ke.ensureKopiaSecret(vmr)
	if err != nil {
		return err
	}

	namespace := ke.vmro.GetNamespace(vmr)
	labels := vmRestoreLabels(vmr, ke.vmro)
	runtime, hasCapacity, err := kopiautil.NewJobRuntime(context.Background(), ke.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}
	job := kopiautil.NewSnapshotCheckJob(kopiautil.SnapshotCheckJobOptions{
		Name:      jobName,
		Namespace: namespace,
		Labels:    labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: harvesterv1.SchemeGroupVersion.String(),
			Kind:       "VirtualMachineRestore",
			Name:       ke.vmro.GetName(vmr),
			UID:        ke.vmro.GetUID(vmr),
		}},
		Runtime:        runtime,
		Tags:           ke.snapshotTags(vmb, vb),
		ConnectCommand: kopiautil.ConnectCommand(),
	})
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
	// Derive the kopia repo password from the S3 secret access key on demand:
	// keeps the BackupTarget setting engine-agnostic, and matches the restic
	// repository credential behavior.
	password, ok := source.Data[util.AWSSecretKey]
	if !ok || len(password) == 0 {
		return "", fmt.Errorf("backup target secret %s/%s is missing %s", source.Namespace, source.Name, util.AWSSecretKey)
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
			kopiautil.PasswordKey: password,
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

func (ke *KopiaRestoreEngine) checkJobName(vmr *harvesterv1.VirtualMachineRestore, vr *harvesterv1.VolumeRestore) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", checkJobNamePrefix, ke.vmro.GetName(vmr), ke.vmro.GetVolRestoreVolumeName(vr)))
}

func (ke *KopiaRestoreEngine) snapshotTags(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) []string {
	vbName := ke.vmbo.GetVolBackupName(vb)
	if vbName == nil {
		return nil
	}
	return []string{
		kopiautil.NamespaceTag(ke.vmbo.GetNamespace(vmb)),
		kopiautil.VMBackupTag(ke.vmbo.GetName(vmb)),
		kopiautil.SnapshotTag(*vbName),
	}
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
