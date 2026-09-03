package restic

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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/backup/common"
	"github.com/harvester/harvester/pkg/backup/engine"
	ctlsnapshotv1 "github.com/harvester/harvester/pkg/generated/controllers/snapshot.storage.k8s.io/v1"
	"github.com/harvester/harvester/pkg/util"
	"github.com/harvester/harvester/pkg/util/jobprogress"
	resticutil "github.com/harvester/harvester/pkg/util/restic"
)

const (
	jobNamePrefix       = "restic-backup"
	forgetJobNamePrefix = "restic-forget"
	checkJobNamePrefix  = "restic-check"
)

type ResticEngine struct {
	vmbo          common.VMBackupOperator
	vsHelper      *common.VolumeSnapshotHelper
	vsClient      ctlsnapshotv1.VolumeSnapshotClient
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

func GetBackupEngine(
	vmbo common.VMBackupOperator,
	vsCache ctlsnapshotv1.VolumeSnapshotCache,
	vsClient ctlsnapshotv1.VolumeSnapshotClient,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	pvcClient ctlcorev1.PersistentVolumeClaimClient,
	secretCache ctlcorev1.SecretCache,
	secretClient ctlcorev1.SecretClient,
	scCache ctlstoragev1.StorageClassCache,
	jobController ctlbatchv1.JobController,
	clientset kubernetes.Interface,
) engine.BackupEngine {
	return &ResticEngine{
		vmbo:          vmbo,
		vsHelper:      common.NewVolumeSnapshotHelper(vsCache, vsClient, nil, nil, vmbo, pvcCache, scCache),
		vsClient:      vsClient,
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

// vmBackupLabels returns the labels stamped on Jobs we create so the Job
// watcher in RegisterWatchers can map a Job event back to its VMBackup.
func vmBackupLabels(vmb *harvesterv1.VirtualMachineBackup, vmbo common.VMBackupOperator) map[string]string {
	return resticutil.JobLabels(map[string]string{
		resticutil.LabelVMBackupNamespace: vmbo.GetNamespace(vmb),
		resticutil.LabelVMBackupName:      vmbo.GetName(vmb),
	})
}

// RegisterWatchers registers a Job event handler that maps Job changes back to
// the owning VMBackup (via labels we stamp on the Job) and enqueues it. This
// removes polling latency — when our backup or forget Job's status changes the
// VMBackup is reconciled immediately.
func (re *ResticEngine) RegisterWatchers(ctx context.Context, enqueue func(namespace, name string)) {
	re.jobController.OnChange(ctx, "restic-backup-job-watcher", func(_ string, job *batchv1.Job) (*batchv1.Job, error) {
		if job == nil {
			return nil, nil
		}
		ns := job.Labels[resticutil.LabelVMBackupNamespace]
		name := job.Labels[resticutil.LabelVMBackupName]
		if ns != "" && name != "" {
			enqueue(ns, name)
		}
		return job, nil
	})
}

func (re *ResticEngine) Reconcile(
	vmb *harvesterv1.VirtualMachineBackup,
	volIndex int,
	vsClassMap map[string]snapshotv1.VolumeSnapshotClass,
) error {
	vb := re.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return fmt.Errorf("volume backup at index %d not found", volIndex)
	}
	if re.vmbo.GetVolBackupReadyToUse(vb) {
		// Backup is persisted in S3; the temporary clone PVC and VolumeSnapshot
		// are no longer needed. Cleanup is idempotent and best-effort — failures
		// are retried on the next reconcile.
		re.cleanupTemporaryResources(vmb, vb)
		return nil
	}
	if ready, err := re.checkRemoteSnapshot(vmb, vb); ready || err != nil {
		return err
	}
	return re.reconcileBackupJob(vmb, vb, vsClassMap)
}

func (re *ResticEngine) reconcileBackupJob(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vsClassMap map[string]snapshotv1.VolumeSnapshotClass,
) error {
	jobName, err := re.jobName(vb)
	if err != nil {
		return err
	}
	job, err := re.jobCache.Get(re.vmbo.GetNamespace(vmb), jobName)
	if err == nil {
		return re.syncFromJob(vb, job)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	if err := re.ensureSnapshot(vmb, vb, vsClassMap); err != nil {
		return err
	}
	clonePVCName := re.clonePVCName(vmb, vb)
	if err := re.ensureClonePVC(vmb, vb, clonePVCName); err != nil {
		return err
	}
	if err := re.createBackupJob(vmb, vb, clonePVCName, jobName); err != nil {
		return err
	}
	return engine.ErrRetryLater
}

func (re *ResticEngine) refreshProgressFromLogs(vb *harvesterv1.VolumeBackup, namespace, jobName string) int {
	current := re.vmbo.GetVolBackupProgress(vb)
	pct, err := jobprogress.FetchProgressPercent(context.Background(), re.clientset, namespace, jobName)
	if err != nil {
		return current
	}
	if err := re.vmbo.SetVolBackupProgress(vb, pct); err != nil {
		return current
	}
	return pct
}

func (re *ResticEngine) UpdateProgress(vb *harvesterv1.VolumeBackup) (int64, error) {
	if re.vmbo.GetVolBackupReadyToUse(vb) {
		return 100, nil
	}
	jobName, jobNamespace, ok := re.jobLocator(vb)
	if !ok {
		return int64(re.vmbo.GetVolBackupProgress(vb)), nil
	}
	return int64(re.refreshProgressFromLogs(vb, jobNamespace, jobName)), nil
}

// ForceDelete drives the per-volume cleanup via the same reconcile pattern as
// Reconcile: each call checks the forget Job's state and either creates it,
// returns ErrRetryLater to await completion, or returns nil to let the caller
// remove finalizers. Once nil is returned the VMBackup CR is finalized and
// K8s cascade-GC reaps the backup Job, clone PVC, and VolumeSnapshot via
// their VMBackup ownerRefs — no explicit cleanup needed here.
func (re *ResticEngine) ForceDelete(vmb *harvesterv1.VirtualMachineBackup, volIndex int) error {
	vb := re.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return nil
	}
	forgetName, err := re.forgetJobName(vb)
	if err != nil {
		return err
	}
	namespace := re.vmbo.GetNamespace(vmb)
	job, err := re.jobCache.Get(namespace, forgetName)
	if err == nil {
		return re.syncFromForgetJob(job)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := re.createForgetJob(vmb, vb, forgetName); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create restic forget job %s/%s: %w", namespace, forgetName, err)
	}
	return engine.ErrRetryLater
}

// syncFromForgetJob maps a forget Job's status to the reconcile contract:
// failure → error (triggers requeue with backoff), still running → ErrRetryLater,
// success → nil (lets the caller finalize the VMBackup).
func (re *ResticEngine) syncFromForgetJob(job *batchv1.Job) error {
	if job.Status.Failed > 0 {
		return fmt.Errorf("restic forget job %s/%s failed", job.Namespace, job.Name)
	}
	if job.Status.Succeeded == 0 {
		return engine.ErrRetryLater
	}
	return nil
}

func (re *ResticEngine) ensureSnapshot(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, vsClassMap map[string]snapshotv1.VolumeSnapshotClass) error {
	vsName := re.volumeSnapshotName(vb)
	vs, err := re.vsHelper.GetVolumeSnapshot(re.vmbo.GetNamespace(vmb), vsName)
	if err != nil {
		return err
	}
	if err := re.vsHelper.CheckSnapshotDeletionStatus(vs, vmb, vb); err != nil {
		return err
	}
	if vs != nil {
		return re.vsHelper.UpdateVolumeBackupStatus(vb, vs)
	}

	if err := re.vsHelper.TryFreezeFS(context.Background(), vmb); err != nil {
		return err
	}

	vsClass := vsClassMap[re.vmbo.GetVolBackupCSIDriver(vb)]
	_, err = re.vsHelper.CreateVolumeSnapshotFromPVC(vmb, vb, &vsClass, re.vsHelper.BuildOwnerReference(vmb))
	return err
}

func (re *ResticEngine) ensureClonePVC(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, clonePVCName string) error {
	namespace := re.vmbo.GetNamespace(vmb)
	pvc, err := re.pvcCache.Get(namespace, clonePVCName)
	if err == nil {
		// WaitForFirstConsumer storage classes only bind once a pod (the backup Job)
		// mounts the PVC, so gating here would deadlock that path.
		if re.waitsForImmediateBinding(pvc) && pvc.Status.Phase != corev1.ClaimBound {
			return engine.ErrRetryLater
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	sourceSpec := re.vmbo.GetVolBackupPVCSpec(vb)
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            clonePVCName,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{re.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: sourceSpec,
	}
	pvc.Spec.VolumeName = ""
	pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: ptr.To("snapshot.storage.k8s.io"),
		Kind:     "VolumeSnapshot",
		Name:     re.volumeSnapshotName(vb),
	}
	pvc.Spec.DataSourceRef = nil
	_, err = re.pvcClient.Create(pvc)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

// waitsForImmediateBinding reports whether the PVC's StorageClass binds eagerly,
// in which case ClaimBound is reachable without a consumer pod. If the SC is
// missing or unreadable we conservatively assume Immediate (the K8s default).
func (re *ResticEngine) waitsForImmediateBinding(pvc *corev1.PersistentVolumeClaim) bool {
	scName := ptr.Deref(pvc.Spec.StorageClassName, "")
	if scName == "" {
		return true
	}
	sc, err := re.scCache.Get(scName)
	if err != nil || sc.VolumeBindingMode == nil {
		return true
	}
	return *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer
}

func (re *ResticEngine) createBackupJob(vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	clonePVCName, jobName string,
) error {
	repository, err := resticutil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := re.ensureResticSecret(vmb)
	if err != nil {
		return err
	}
	labels := vmBackupLabels(vmb, re.vmbo)
	runtime, hasCapacity, err := resticutil.NewJobRuntime(context.Background(), re.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}

	namespace := re.vmbo.GetNamespace(vmb)
	pvName := re.vmbo.GetVolBackupPVName(vb)
	volumes := runtime.VolumesWith(corev1.Volume{
		Name: "volume",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: clonePVCName},
		},
	})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{re.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](60),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:         "backup",
						Image:        runtime.Image,
						Env:          runtime.Env,
						Command:      []string{"/bin/sh", "-c"},
						Resources:    runtime.Resources,
						VolumeMounts: runtime.VolumeMounts,
						Args: []string{fmt.Sprintf(
							// Ensure the repo exists before backing up. `restic snapshots` is the
							// canonical "is this a repo?" probe; on a fresh S3 prefix it fails and
							// we initialize. If another volume job wins the init race, the follow-up
							// snapshots probe accepts that as success.
							// pipefail propagates a failing io-mode read; without it restic backup
							// would happily store an empty snapshot from empty stdin.
							"set -eo pipefail; "+
								"(%s snapshots > /dev/null 2>&1 || { %s init || %s snapshots > /dev/null 2>&1; }) && "+
								"/usr/bin/harvester io-mode -device /dev/%s -mode=read | %s -q backup --stdin --stdin-filename %s --tag=%s,%s,%s",
							runtime.Command,
							runtime.Command,
							runtime.Command,
							clonePVCName,
							runtime.Command,
							pvName,
							resticutil.NamespaceTag(namespace),
							resticutil.VMBackupTag(re.vmbo.GetName(vmb)),
							resticutil.SnapshotTag(*re.vmbo.GetVolBackupName(vb)),
						)},
						VolumeDevices: []corev1.VolumeDevice{{
							Name:       "volume",
							DevicePath: "/dev/" + clonePVCName,
						}},
					}},
					Volumes: volumes,
				},
			},
		},
	}
	_, err = re.jobClient.Create(job)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (re *ResticEngine) checkRemoteSnapshot(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) (bool, error) {
	checkName, err := re.checkJobName(vb)
	if err != nil {
		return false, err
	}
	namespace := re.vmbo.GetNamespace(vmb)
	result, err := resticutil.CheckSnapshotJob(
		context.Background(),
		re.clientset,
		checkName,
		func(name string) (*batchv1.Job, error) {
			return re.jobCache.Get(namespace, name)
		},
		func(name string) error {
			return re.createCheckJob(vmb, vb, name)
		},
	)
	if err != nil {
		return false, err
	}
	discoveredBackup := re.isDiscoveredBackup(vmb)
	switch result {
	case resticutil.SnapshotCheckPending:
		return false, engine.ErrRetryLater
	case resticutil.SnapshotCheckMissing:
		if discoveredBackup {
			return false, fmt.Errorf("restic snapshot for discovered backup %s/%s not found", namespace, checkName)
		}
		return false, nil
	case resticutil.SnapshotCheckFailed:
		if discoveredBackup {
			return false, fmt.Errorf("restic snapshot check job %s/%s failed", namespace, checkName)
		}
		return false, nil
	case resticutil.SnapshotCheckFound:
		return re.markReady(vb)
	default:
		return false, fmt.Errorf("unknown restic snapshot check result %q", result)
	}
}

func (re *ResticEngine) isDiscoveredBackup(vmb *harvesterv1.VirtualMachineBackup) bool {
	return re.vmbo.GetSourceUID(vmb) == nil
}

func (re *ResticEngine) markReady(vb *harvesterv1.VolumeBackup) (bool, error) {
	now := metav1.Now()
	ready := true
	if err := re.vmbo.SetVolBackupReadyToUse(vb, &ready); err != nil {
		return false, err
	}
	if err := re.vmbo.SetVolBackupCreationTime(vb, &now); err != nil {
		return false, err
	}
	if err := re.vmbo.SetVolBackupProgress(vb, 100); err != nil {
		return false, err
	}
	return true, nil
}

func (re *ResticEngine) createCheckJob(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, jobName string) error {
	repository, err := resticutil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := re.ensureResticSecret(vmb)
	if err != nil {
		return err
	}

	namespace := re.vmbo.GetNamespace(vmb)
	vbName := re.vmbo.GetVolBackupName(vb)
	if vbName == nil || *vbName == "" {
		return fmt.Errorf("volume backup name is nil")
	}
	labels := vmBackupLabels(vmb, re.vmbo)
	runtime, hasCapacity, err := resticutil.NewJobRuntime(context.Background(), re.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}
	job := resticutil.NewSnapshotCheckJob(resticutil.SnapshotCheckJobOptions{
		Name:            jobName,
		Namespace:       namespace,
		Labels:          labels,
		OwnerReferences: []metav1.OwnerReference{re.vsHelper.BuildOwnerReference(vmb)},
		Runtime:         runtime,
		Tags: []string{
			resticutil.NamespaceTag(namespace),
			resticutil.VMBackupTag(re.vmbo.GetName(vmb)),
			resticutil.SnapshotTag(*vbName),
		},
	})
	_, err = re.jobClient.Create(job)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

// createForgetJob fires a one-shot job that runs `restic forget --prune` for
// the snapshot(s) matching this VolumeBackup. Mirrors createBackupJob's
// resource shape (same namespace, same per-VMBackup credential secret) so it
// inherits the same lifecycle assumptions: Pod env from secretKeyRef is
// resolved at startup, so the Pod survives the secret being GC'd once the
// VMBackup CR is finalized.
func (re *ResticEngine) createForgetJob(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, jobName string) error {
	repository, err := resticutil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := re.ensureResticSecret(vmb)
	if err != nil {
		return err
	}

	namespace := re.vmbo.GetNamespace(vmb)
	vbName := re.vmbo.GetVolBackupName(vb)
	if vbName == nil || *vbName == "" {
		return nil
	}
	labels := vmBackupLabels(vmb, re.vmbo)
	runtime, hasCapacity, err := resticutil.NewJobRuntime(context.Background(), re.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{re.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](60),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:         "forget",
						Image:        runtime.Image,
						Env:          runtime.Env,
						Command:      []string{"/bin/sh", "-c"},
						Resources:    runtime.Resources,
						VolumeMounts: runtime.VolumeMounts,
						// Comma-separated tags in a single --tag flag = AND. If the repo
						// doesn't exist (backup never pushed), short-circuit to success —
						// nothing to forget.
						Args: []string{fmt.Sprintf(
							// `--keep-last 0` is treated as "no policy specified" by restic and
							// would refuse to remove anything; `--unsafe-allow-remove-all` (added
							// in restic 0.17) is the explicit opt-in to drop every matching
							// snapshot. Safe here because the three tag filters AND together
							// to scope removal to exactly this VolumeBackup.
							"%s snapshots > /dev/null 2>&1 || exit 0; "+
								"%s forget --tag %s,%s,%s --unsafe-allow-remove-all --prune",
							runtime.Command,
							runtime.Command,
							resticutil.NamespaceTag(namespace),
							resticutil.VMBackupTag(re.vmbo.GetName(vmb)),
							resticutil.SnapshotTag(*vbName),
						)},
					}},
					Volumes: runtime.Volumes,
				},
			},
		},
	}
	_, err = re.jobClient.Create(job)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (re *ResticEngine) ensureResticSecret(vmb *harvesterv1.VirtualMachineBackup) (string, error) {
	source, err := re.secretCache.Get(util.LonghornSystemNamespaceName, util.BackupTargetSecretName)
	if err != nil {
		return "", err
	}
	// Derive the restic repo password from the S3 secret access key on demand:
	// keeps the BackupTarget setting engine-agnostic, and the credential is
	// already scoped to the same trust boundary as the repo itself.
	password, ok := source.Data[util.AWSSecretKey]
	if !ok || len(password) == 0 {
		return "", fmt.Errorf("backup target secret %s/%s is missing %s", source.Namespace, source.Name, util.AWSSecretKey)
	}

	namespace := re.vmbo.GetNamespace(vmb)
	name := strings.ToLower(fmt.Sprintf("restic-credentials-%s", re.vmbo.GetName(vmb)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{re.vsHelper.BuildOwnerReference(vmb)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			util.AWSAccessKey:      source.Data[util.AWSAccessKey],
			util.AWSSecretKey:      source.Data[util.AWSSecretKey],
			resticutil.PasswordKey: password,
		},
	}
	if _, err := re.secretClient.Create(secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return name, nil
}

func (re *ResticEngine) syncFromJob(vb *harvesterv1.VolumeBackup, job *batchv1.Job) error {
	if job.Status.Failed > 0 {
		return fmt.Errorf("restic backup job %s/%s failed", job.Namespace, job.Name)
	}
	if job.Status.Succeeded == 0 {
		// Job still running — update progress from io-mode pod logs (best effort).
		if pct, err := jobprogress.FetchProgressPercent(context.Background(), re.clientset, job.Namespace, job.Name); err == nil {
			_ = re.vmbo.SetVolBackupProgress(vb, pct)
		}
		return engine.ErrRetryLater
	}
	now := metav1.Now()
	ready := true
	if err := re.vmbo.SetVolBackupReadyToUse(vb, &ready); err != nil {
		return err
	}
	if err := re.vmbo.SetVolBackupCreationTime(vb, &now); err != nil {
		return err
	}
	if err := re.vmbo.SetVolBackupProgress(vb, 100); err != nil {
		return err
	}
	// Cleanup is deferred to the next reconcile (when ReadyToUse is persisted)
	// to avoid a race where Update fails after we've deleted the VS/clone PVC.
	logrus.Debugf("restic backup job %s/%s completed", job.Namespace, job.Name)
	return nil
}

// cleanupTemporaryResources removes the backup Job, cloned PVC, and
// VolumeSnapshot created for this backup. Called after ReadyToUse is
// persisted, so the Job has already succeeded (otherwise we wouldn't be
// here) and failure of any step is safe to retry on the next reconcile.
//
// Order matters: delete the Job first so its Pod is GC'd (Background
// propagation), otherwise the clone PVC would stay stuck on the
// kubernetes.io/pvc-protection finalizer until TTLSecondsAfterFinished
// reaped the Pod about a minute later.
func (re *ResticEngine) cleanupTemporaryResources(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) {
	namespace := re.vmbo.GetNamespace(vmb)
	re.deleteBackupJob(namespace, vb)

	clonePVCName := re.clonePVCName(vmb, vb)
	if err := re.pvcClient.Delete(namespace, clonePVCName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		logrus.Warnf("failed to delete restic temporary PVC %s/%s: %v", namespace, clonePVCName, err)
	}
	if vsName := re.volumeSnapshotName(vb); vsName != "" {
		if err := re.vsClient.Delete(namespace, vsName, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			logrus.Warnf("failed to delete restic temporary VolumeSnapshot %s/%s: %v", namespace, vsName, err)
		}
	}
}

func (re *ResticEngine) deleteBackupJob(namespace string, vb *harvesterv1.VolumeBackup) {
	jobName, err := re.jobName(vb)
	if err != nil {
		return
	}

	err = re.jobClient.Delete(namespace, jobName, &metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})

	if err == nil || apierrors.IsNotFound(err) {
		return
	}

	logrus.Warnf("failed to delete restic backup job %s/%s: %v", namespace, jobName, err)
}

func (re *ResticEngine) volumeSnapshotName(vb *harvesterv1.VolumeBackup) string {
	if name := re.vmbo.GetVolBackupName(vb); name != nil {
		return *name
	}
	return ""
}

func (re *ResticEngine) clonePVCName(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-restic-clone", re.vmbo.GetName(vmb), re.vmbo.GetVolBackupPVCName(vb)))
}

func (re *ResticEngine) jobName(vb *harvesterv1.VolumeBackup) (string, error) {
	name := re.vmbo.GetVolBackupName(vb)
	if name == nil {
		return "", fmt.Errorf("volume backup name is nil")
	}
	return strings.ToLower(fmt.Sprintf("%s-%s", jobNamePrefix, *name)), nil
}

func (re *ResticEngine) forgetJobName(vb *harvesterv1.VolumeBackup) (string, error) {
	name := re.vmbo.GetVolBackupName(vb)
	if name == nil {
		return "", fmt.Errorf("volume backup name is nil")
	}
	return strings.ToLower(fmt.Sprintf("%s-%s", forgetJobNamePrefix, *name)), nil
}

func (re *ResticEngine) checkJobName(vb *harvesterv1.VolumeBackup) (string, error) {
	name := re.vmbo.GetVolBackupName(vb)
	if name == nil {
		return "", fmt.Errorf("volume backup name is nil")
	}
	return strings.ToLower(fmt.Sprintf("%s-%s", checkJobNamePrefix, *name)), nil
}

// jobLocator returns the name/namespace to look up the backup Job for vb,
// or false if the volume backup name isn't set yet. Used by UpdateProgress
// which has only a VolumeBackup handle (no enclosing VMBackup). The Job is
// created in the same namespace as the source PVC.
func (re *ResticEngine) jobLocator(vb *harvesterv1.VolumeBackup) (name, namespace string, ok bool) {
	vbName := re.vmbo.GetVolBackupName(vb)
	if vbName == nil {
		return "", "", false
	}
	ns := re.vmbo.GetVolBackupPVCNameSpace(vb)
	if ns == "" {
		return "", "", false
	}
	return strings.ToLower(fmt.Sprintf("%s-%s", jobNamePrefix, *vbName)), ns, true
}
