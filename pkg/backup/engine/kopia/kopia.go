package kopia

import (
	"context"
	"fmt"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	ctlbatchv1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/batch/v1"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/storage/v1"
	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/backup/common"
	"github.com/harvester/harvester/pkg/backup/engine"
	ctlsnapshotv1 "github.com/harvester/harvester/pkg/generated/controllers/snapshot.storage.k8s.io/v1"
	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
	"github.com/harvester/harvester/pkg/util/jobprogress"
	kopiautil "github.com/harvester/harvester/pkg/util/kopia"
)

const (
	jobNamePrefix = "kopia-backup"
)

type KopiaEngine struct {
	vmbo         common.VMBackupOperator
	vsHelper     *common.VolumeSnapshotHelper
	vsClient     ctlsnapshotv1.VolumeSnapshotClient
	pvcCache     ctlcorev1.PersistentVolumeClaimCache
	pvcClient    ctlcorev1.PersistentVolumeClaimClient
	secretCache  ctlcorev1.SecretCache
	secretClient ctlcorev1.SecretClient
	jobCache     ctlbatchv1.JobCache
	jobClient    ctlbatchv1.JobClient
	clientset    kubernetes.Interface
}

func GetBackupEngine(
	vmbo common.VMBackupOperator,
	vsCache ctlsnapshotv1.VolumeSnapshotCache,
	vsClient ctlsnapshotv1.VolumeSnapshotClient,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	pvcClient ctlcorev1.PersistentVolumeClaimClient,
	secretCache ctlcorev1.SecretCache,
	secretClient ctlcorev1.SecretClient,
	scCache ctlstoragev1.StorageClassCache,
	jobCache ctlbatchv1.JobCache,
	jobClient ctlbatchv1.JobClient,
	clientset kubernetes.Interface,
) engine.BackupEngine {
	return &KopiaEngine{
		vmbo:         vmbo,
		vsHelper:     common.NewVolumeSnapshotHelper(vsCache, vsClient, nil, nil, vmbo, pvcCache, scCache),
		vsClient:     vsClient,
		pvcCache:     pvcCache,
		pvcClient:    pvcClient,
		secretCache:  secretCache,
		secretClient: secretClient,
		jobCache:     jobCache,
		jobClient:    jobClient,
		clientset:    clientset,
	}
}

func (ke *KopiaEngine) Reconcile(
	vmb *harvesterv1.VirtualMachineBackup,
	volIndex int,
	vsClassMap map[string]snapshotv1.VolumeSnapshotClass,
) error {
	vb := ke.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return fmt.Errorf("volume backup at index %d not found", volIndex)
	}
	if ke.vmbo.GetVolBackupReadyToUse(vb) {
		// Backup is persisted in S3; the temporary clone PVC and VolumeSnapshot
		// are no longer needed. Cleanup is idempotent and best-effort — failures
		// are retried on the next reconcile.
		ke.cleanupTemporaryResources(vmb, vb)
		return nil
	}

	jobName, err := ke.jobName(vmb, vb)
	if err != nil {
		return err
	}
	job, err := ke.jobCache.Get(ke.vmbo.GetNamespace(vmb), jobName)
	if err == nil {
		return ke.syncFromJob(vmb, vb, job)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	if err := ke.ensureSnapshot(vmb, vb, vsClassMap); err != nil {
		return err
	}
	clonePVCName := ke.clonePVCName(vmb, vb)
	if err := ke.ensureClonePVC(vmb, vb, clonePVCName); err != nil {
		return err
	}
	if err := ke.createBackupJob(vmb, vb, clonePVCName, jobName); err != nil {
		return err
	}
	return engine.ErrRetryLater
}

func (ke *KopiaEngine) UpdateProgress(vb *harvesterv1.VolumeBackup) (int64, error) {
	if ke.vmbo.GetVolBackupReadyToUse(vb) {
		return 100, nil
	}
	jobName, jobNamespace, ok := ke.jobLocator(vb)
	if !ok {
		return int64(ke.vmbo.GetVolBackupProgress(vb)), nil
	}
	pct, err := jobprogress.FetchProgressPercent(context.Background(), ke.clientset, jobNamespace, jobName)
	if err != nil {
		return int64(ke.vmbo.GetVolBackupProgress(vb)), nil
	}
	if err := ke.vmbo.SetVolBackupProgress(vb, pct); err != nil {
		return 0, err
	}
	return int64(pct), nil
}

func (ke *KopiaEngine) ForceDelete(vmb *harvesterv1.VirtualMachineBackup, volIndex int) error {
	vb := ke.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return nil
	}
	namespace := ke.vmbo.GetNamespace(vmb)
	if jobName, err := ke.jobName(vmb, vb); err == nil {
		if err := ke.jobClient.Delete(namespace, jobName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	clonePVCName := ke.clonePVCName(vmb, vb)
	if err := ke.pvcClient.Delete(namespace, clonePVCName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		logrus.Debugf("failed to delete kopia temporary PVC %s/%s: %v", namespace, clonePVCName, err)
	}
	if vsName := ke.volumeSnapshotName(vb); vsName != "" {
		if err := ke.vsClient.Delete(namespace, vsName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// RegisterWatchers is a no-op for now. Once Kopia adopts the same
// label-and-watch pattern as the restic engine, it can install a Job
// watcher here to eliminate poll-based requeues.
func (ke *KopiaEngine) RegisterWatchers(_ context.Context, _ func(string, string)) {}

func (ke *KopiaEngine) ensureSnapshot(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, vsClassMap map[string]snapshotv1.VolumeSnapshotClass) error {
	vsName := ke.volumeSnapshotName(vb)
	vs, err := ke.vsHelper.GetVolumeSnapshot(ke.vmbo.GetNamespace(vmb), vsName)
	if err != nil {
		return err
	}
	if err := ke.vsHelper.CheckSnapshotDeletionStatus(vs, vmb, vb); err != nil {
		return err
	}
	if vs != nil {
		return ke.vsHelper.UpdateVolumeBackupStatus(vb, vs)
	}

	if err := ke.vsHelper.TryFreezeFS(context.Background(), vmb); err != nil {
		return err
	}

	vsClass := vsClassMap[ke.vmbo.GetVolBackupCSIDriver(vb)]
	_, err = ke.vsHelper.CreateVolumeSnapshotFromPVC(vmb, vb, &vsClass, ke.vsHelper.BuildOwnerReference(vmb))
	return err
}

func (ke *KopiaEngine) ensureClonePVC(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, clonePVCName string) error {
	namespace := ke.vmbo.GetNamespace(vmb)
	pvc, err := ke.pvcCache.Get(namespace, clonePVCName)
	if err == nil {
		if pvc.Status.Phase != corev1.ClaimBound {
			return engine.ErrRetryLater
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	sourceSpec := ke.vmbo.GetVolBackupPVCSpec(vb)
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            clonePVCName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ke.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: sourceSpec,
	}
	pvc.Spec.VolumeName = ""
	pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: ptr.To("snapshot.storage.k8s.io"),
		Kind:     "VolumeSnapshot",
		Name:     ke.volumeSnapshotName(vb),
	}
	pvc.Spec.DataSourceRef = nil
	_, err = ke.pvcClient.Create(pvc)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

func (ke *KopiaEngine) createBackupJob(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, clonePVCName, jobName string) error {
	target, err := settings.DecodeBackupTarget(settings.BackupTargetSet.Get())
	if err != nil {
		return err
	}
	repository, err := kopiautil.S3Repository(target)
	if err != nil {
		return err
	}
	secretName, err := ke.ensureKopiaSecret(vmb)
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

	namespace := ke.vmbo.GetNamespace(vmb)
	pvName := ke.vmbo.GetVolBackupPVName(vb)
	command := fmt.Sprintf(
		"set -e\n%s\n/usr/bin/harvester io-mode -device /dev/%s -mode=read | kopia --config-file=%s snapshot create --stdin-file /%s --tags %s,%s -",
		kopiautil.ConnectOrCreateCommand(),
		clonePVCName,
		kopiautil.ConfigPath,
		pvName,
		kopiautil.NamespaceTag(namespace),
		kopiautil.SnapshotTag(*vbName),
	)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ke.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](300),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "backup",
						Image:   image,
						Env:     kopiautil.Env(secretName, repository),
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{command},
						VolumeDevices: []corev1.VolumeDevice{{
							Name:       "volume",
							DevicePath: "/dev/" + clonePVCName,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "volume",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: clonePVCName},
						},
					}},
				},
			},
		},
	}
	_, err = ke.jobClient.Create(job)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (ke *KopiaEngine) ensureKopiaSecret(vmb *harvesterv1.VirtualMachineBackup) (string, error) {
	source, err := ke.secretCache.Get(util.LonghornSystemNamespaceName, util.BackupTargetSecretName)
	if err != nil {
		return "", err
	}
	if _, ok := source.Data[kopiautil.PasswordKey]; !ok {
		return "", fmt.Errorf("backup target secret %s/%s is missing %s", source.Namespace, source.Name, kopiautil.PasswordKey)
	}

	namespace := ke.vmbo.GetNamespace(vmb)
	name := strings.ToLower(fmt.Sprintf("kopia-credentials-%s", ke.vmbo.GetName(vmb)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{ke.vsHelper.BuildOwnerReference(vmb)},
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

func (ke *KopiaEngine) syncFromJob(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, job *batchv1.Job) error {
	if job.Status.Failed > 0 {
		return fmt.Errorf("kopia backup job %s/%s failed", job.Namespace, job.Name)
	}
	if job.Status.Succeeded == 0 {
		// Job still running — update progress from io-mode pod logs (best effort).
		if pct, err := jobprogress.FetchProgressPercent(context.Background(), ke.clientset, job.Namespace, job.Name); err == nil {
			_ = ke.vmbo.SetVolBackupProgress(vb, pct)
		}
		return engine.ErrRetryLater
	}
	now := metav1.Now()
	ready := true
	if err := ke.vmbo.SetVolBackupReadyToUse(vb, &ready); err != nil {
		return err
	}
	if err := ke.vmbo.SetVolBackupCreationTime(vb, &now); err != nil {
		return err
	}
	if err := ke.vmbo.SetVolBackupProgress(vb, 100); err != nil {
		return err
	}
	// Cleanup is deferred to the next reconcile (when ReadyToUse is persisted)
	// to avoid a race where Update fails after we've deleted the VS/clone PVC.
	logrus.Debugf("kopia backup job %s/%s completed", job.Namespace, job.Name)
	return nil
}

// cleanupTemporaryResources removes the VolumeSnapshot and cloned PVC created
// for this backup. Called after ReadyToUse is persisted, so failure here is
// safe to retry on the next reconcile. The Job itself is left to be GC'd by
// TTLSecondsAfterFinished — deleting it here would break retry logic if the
// VMB Update lagged the deletion.
func (ke *KopiaEngine) cleanupTemporaryResources(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) {
	namespace := ke.vmbo.GetNamespace(vmb)
	clonePVCName := ke.clonePVCName(vmb, vb)
	if err := ke.pvcClient.Delete(namespace, clonePVCName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		logrus.Warnf("failed to delete kopia temporary PVC %s/%s: %v", namespace, clonePVCName, err)
	}
	if vsName := ke.volumeSnapshotName(vb); vsName != "" {
		if err := ke.vsClient.Delete(namespace, vsName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			logrus.Warnf("failed to delete kopia temporary VolumeSnapshot %s/%s: %v", namespace, vsName, err)
		}
	}
}

func (ke *KopiaEngine) volumeSnapshotName(vb *harvesterv1.VolumeBackup) string {
	if name := ke.vmbo.GetVolBackupName(vb); name != nil {
		return *name
	}
	return ""
}

func (ke *KopiaEngine) clonePVCName(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-kopia-clone", ke.vmbo.GetName(vmb), ke.vmbo.GetVolBackupPVCName(vb)))
}

func (ke *KopiaEngine) jobName(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) (string, error) {
	name := ke.vmbo.GetVolBackupName(vb)
	if name == nil {
		return "", fmt.Errorf("volume backup name is nil")
	}
	return strings.ToLower(fmt.Sprintf("%s-%s", jobNamePrefix, *name)), nil
}

// jobLocator returns the name/namespace to look up the backup Job for vb,
// or false if the volume backup name isn't set yet. See restic.jobLocator.
func (ke *KopiaEngine) jobLocator(vb *harvesterv1.VolumeBackup) (name, namespace string, ok bool) {
	vbName := ke.vmbo.GetVolBackupName(vb)
	if vbName == nil {
		return "", "", false
	}
	ns := ke.vmbo.GetVolBackupPVCNameSpace(vb)
	if ns == "" {
		return "", "", false
	}
	return strings.ToLower(fmt.Sprintf("%s-%s", jobNamePrefix, *vbName)), ns, true
}
