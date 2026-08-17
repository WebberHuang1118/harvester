package restic

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
	resticutil "github.com/harvester/harvester/pkg/util/restic"
)

const (
	jobNamePrefix      = "restic-restore"
	checkJobNamePrefix = "restic-restore-check"
)

type ResticRestoreEngine struct {
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
	return &ResticRestoreEngine{
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
	return resticutil.JobLabels(map[string]string{
		resticutil.LabelVMRestoreNamespace: vmro.GetNamespace(vmr),
		resticutil.LabelVMRestoreName:      vmro.GetName(vmr),
	})
}

// RegisterWatchers registers a Job event handler that maps Job changes back to
// the owning VMRestore (via labels we stamp on the Job) and enqueues it.
func (re *ResticRestoreEngine) RegisterWatchers(ctx context.Context, enqueue func(namespace, name string)) {
	re.jobController.OnChange(ctx, "restic-restore-job-watcher", func(_ string, job *batchv1.Job) (*batchv1.Job, error) {
		if job == nil {
			return nil, nil
		}
		ns := job.Labels[resticutil.LabelVMRestoreNamespace]
		name := job.Labels[resticutil.LabelVMRestoreName]
		if ns != "" && name != "" {
			enqueue(ns, name)
		}
		return job, nil
	})
}

func (re *ResticRestoreEngine) Reconcile(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	volIndex int,
) error {
	vr := re.vmro.GetVolRestore(vmr, volIndex)
	if vr == nil {
		return fmt.Errorf("volume restore at index %d not found", volIndex)
	}
	// Once a volume has finished restoring (progress=100), bail out before any
	// Job lookup. The job watcher also fires when TTL or owner-reference garbage
	// collection removes the Job; this prevents that event from recreating the
	// Job and running the restore a second time.
	if re.vmro.GetVolRestoreProgress(vr) == 100 {
		return nil
	}
	vb := re.vmbo.GetVolBackup(vmb, volIndex)
	if vb == nil {
		return fmt.Errorf("volume backup at index %d not found", volIndex)
	}

	if err := re.checkRemoteSnapshot(vmr, vmb, vr, vb); err != nil {
		return err
	}
	if err := re.ensurePVC(vmr, vr, vb); err != nil {
		return err
	}

	jobName := re.jobName(vmr, vr)
	job, err := re.jobCache.Get(re.vmro.GetNamespace(vmr), jobName)
	if err == nil {
		return re.syncFromJob(vr, job)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := re.createRestoreJob(vmr, vmb, vr, vb, jobName); err != nil {
		return err
	}
	return engine.ErrRetryLater
}

func (re *ResticRestoreEngine) UpdateProgress(vr *harvesterv1.VolumeRestore) (int64, error) {
	current := re.vmro.GetVolRestoreProgress(vr)
	if current == 100 {
		return 100, nil
	}
	jobName, namespace, ok := re.jobLocator(vr)
	if !ok {
		return int64(current), nil
	}
	return int64(re.refreshProgressFromLogs(vr, namespace, jobName)), nil
}

// Delete is a no-op: completed restore Jobs are removed by
// TTLSecondsAfterFinished, while unfinished Jobs carry an OwnerReference to
// the VirtualMachineRestore and are removed by cascading garbage collection.
func (re *ResticRestoreEngine) Delete(_ *harvesterv1.VirtualMachineRestore, _ int) error {
	return nil
}

// waitsForImmediateBinding reports whether the PVC's StorageClass binds eagerly,
// in which case ClaimBound is reachable without a consumer pod. If the SC is
// missing or unreadable we conservatively assume Immediate (the K8s default).
func (re *ResticRestoreEngine) waitsForImmediateBinding(pvc *corev1.PersistentVolumeClaim) bool {
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

func (re *ResticRestoreEngine) ensurePVC(
	vmr *harvesterv1.VirtualMachineRestore,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
) error {
	namespace := re.vmro.GetNamespace(vmr)
	pvcName := re.vmro.GetVolRestorePVCName(vr)
	pvc, err := re.pvcCache.Get(namespace, pvcName)
	if err == nil {
		// WaitForFirstConsumer storage classes only bind once a pod (the restore Job)
		// mounts the PVC, so gating here would deadlock that path.
		if re.waitsForImmediateBinding(pvc) && pvc.Status.Phase != corev1.ClaimBound {
			return engine.ErrRetryLater
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	pvcSpec := re.vmbo.GetVolBackupPVCSpec(vb)
	annotations := pvchelper.BuildRestoreAnnotations(
		re.vmbo.GetVolBackupPVCAnnotations(vb),
		re.vmro.GetName(vmr),
		restorecommon.RestoreNameAnnotation,
	)
	labels := pvchelper.BuildRestoreLabels(re.vmbo.GetVolBackupPVCLabels(vb))
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
		Name:       re.vmro.GetName(vmr),
		UID:        re.vmro.GetUID(vmr),
	}}
	pvc.Spec.VolumeName = ""
	pvc.Spec.DataSource = nil
	pvc.Spec.DataSourceRef = nil

	_, err = re.pvcClient.Create(pvc)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

func (re *ResticRestoreEngine) createRestoreJob(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
	jobName string,
) error {
	repository, err := resticutil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := re.ensureResticSecret(vmr)
	if err != nil {
		return err
	}
	vbName := re.vmbo.GetVolBackupName(vb)
	if vbName == nil {
		return fmt.Errorf("volume backup name is nil")
	}

	namespace := re.vmro.GetNamespace(vmr)
	pvcName := re.vmro.GetVolRestorePVCName(vr)
	pvName := re.vmbo.GetVolBackupPVName(vb)
	labels := vmRestoreLabels(vmr, re.vmro)
	runtime, hasCapacity, err := resticutil.NewJobRuntime(context.Background(), re.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}
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
				Name:       re.vmro.GetName(vmr),
				UID:        re.vmro.GetUID(vmr),
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
						Args: []string{fmt.Sprintf(
							// pipefail propagates a failing `restic dump` through the pipeline;
							// without it io-mode's clean exit on empty stdin would hide the
							// failure and leave the PVC blank. Leading `/` on the dump path
							// matches the absolute path restic stored via --stdin-filename.
							"set -eo pipefail; "+
								"%s -q dump --tag=%s,%s,%s latest /%s | /usr/bin/harvester io-mode -device /dev/%s -mode=write",
							runtime.Command,
							resticutil.NamespaceTag(re.vmbo.GetNamespace(vmb)),
							resticutil.VMBackupTag(re.vmbo.GetName(vmb)),
							resticutil.SnapshotTag(*vbName),
							pvName,
							pvcName,
						)},
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
	_, err = re.jobClient.Create(job)
	if apierrors.IsAlreadyExists(err) {
		return engine.ErrRetryLater
	}
	return err
}

func (re *ResticRestoreEngine) checkRemoteSnapshot(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vr *harvesterv1.VolumeRestore,
	vb *harvesterv1.VolumeBackup,
) error {
	checkName := re.checkJobName(vmr, vr)
	namespace := re.vmro.GetNamespace(vmr)
	result, err := resticutil.CheckSnapshotJob(
		context.Background(),
		re.clientset,
		checkName,
		func(name string) (*batchv1.Job, error) {
			return re.jobCache.Get(namespace, name)
		},
		func(name string) error {
			return re.createCheckJob(vmr, vmb, vb, name)
		},
	)
	if err != nil {
		return err
	}
	switch result {
	case resticutil.SnapshotCheckPending:
		return engine.ErrRetryLater
	case resticutil.SnapshotCheckMissing:
		return fmt.Errorf("restic snapshot for restore job %s/%s not found", namespace, checkName)
	case resticutil.SnapshotCheckFailed:
		return fmt.Errorf("restic snapshot check job %s/%s failed", namespace, checkName)
	case resticutil.SnapshotCheckFound:
		return nil
	default:
		return fmt.Errorf("unknown restic snapshot check result %q", result)
	}
}

func (re *ResticRestoreEngine) createCheckJob(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	jobName string,
) error {
	repository, err := resticutil.RepositoryFromSetting()
	if err != nil {
		return err
	}
	secretName, err := re.ensureResticSecret(vmr)
	if err != nil {
		return err
	}
	vbName := re.vmbo.GetVolBackupName(vb)
	if vbName == nil {
		return fmt.Errorf("volume backup name is nil")
	}

	namespace := re.vmro.GetNamespace(vmr)
	labels := vmRestoreLabels(vmr, re.vmro)
	runtime, hasCapacity, err := resticutil.NewJobRuntime(context.Background(), re.clientset, secretName, repository, labels)
	if err != nil {
		return err
	}
	if !hasCapacity {
		return engine.ErrRetryLater
	}
	job := resticutil.NewSnapshotCheckJob(resticutil.SnapshotCheckJobOptions{
		Name:      jobName,
		Namespace: namespace,
		Labels:    labels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: harvesterv1.SchemeGroupVersion.String(),
			Kind:       "VirtualMachineRestore",
			Name:       re.vmro.GetName(vmr),
			UID:        re.vmro.GetUID(vmr),
		}},
		Runtime: runtime,
		Tags: []string{
			resticutil.NamespaceTag(re.vmbo.GetNamespace(vmb)),
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

func (re *ResticRestoreEngine) ensureResticSecret(vmr *harvesterv1.VirtualMachineRestore) (string, error) {
	source, err := re.secretCache.Get(util.LonghornSystemNamespaceName, util.BackupTargetSecretName)
	if err != nil {
		return "", err
	}
	// Mirror the backup-side derivation: use the S3 secret access key as the
	// repo password so backup and restore agree without a setting field.
	password, ok := source.Data[util.AWSSecretKey]
	if !ok || len(password) == 0 {
		return "", fmt.Errorf("backup target secret %s/%s is missing %s", source.Namespace, source.Name, util.AWSSecretKey)
	}

	namespace := re.vmro.GetNamespace(vmr)
	name := strings.ToLower(fmt.Sprintf("restic-credentials-%s", re.vmro.GetName(vmr)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: harvesterv1.SchemeGroupVersion.String(),
				Kind:       "VirtualMachineRestore",
				Name:       re.vmro.GetName(vmr),
				UID:        re.vmro.GetUID(vmr),
			}},
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

// syncFromJob reports only state transitions: failure → error, still running
// → ErrRetryLater, and success → mark progress=100. Intermediate progress
// sampling lives in UpdateProgress so it runs every reconcile via the
// controller's updateProgressMetrics, independent of this path. Job cleanup
// is left to TTLSecondsAfterFinished.
func (re *ResticRestoreEngine) syncFromJob(vr *harvesterv1.VolumeRestore, job *batchv1.Job) error {
	if job.Status.Failed > 0 {
		return fmt.Errorf("restic restore job %s/%s failed", job.Namespace, job.Name)
	}
	if job.Status.Succeeded == 0 {
		return engine.ErrRetryLater
	}
	if err := re.vmro.SetVolRestoreProgress(vr, 100); err != nil {
		return err
	}
	// Leave Job cleanup to TTLSecondsAfterFinished. Deleting it here would
	// race with the Job-watcher: the deletion event re-fires Reconcile while
	// the VMR informer cache may still show progress<100 and the Job cache
	// may show NotFound, leading us to recreate the Job and run the restore
	// a second time. By the time TTL evicts the Job, progress=100 has long
	// been visible in cache and Reconcile early-returns.
	logrus.Debugf("restic restore job %s/%s completed", job.Namespace, job.Name)
	return nil
}

func (re *ResticRestoreEngine) refreshProgressFromLogs(vr *harvesterv1.VolumeRestore, namespace, jobName string) int {
	current := re.vmro.GetVolRestoreProgress(vr)
	pct, err := jobprogress.FetchProgressPercent(context.Background(), re.clientset, namespace, jobName)
	if err != nil {
		return current
	}
	if err := re.vmro.SetVolRestoreProgress(vr, pct); err != nil {
		return current
	}
	return pct
}

func (re *ResticRestoreEngine) jobName(vmr *harvesterv1.VirtualMachineRestore, vr *harvesterv1.VolumeRestore) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", jobNamePrefix, re.vmro.GetName(vmr), re.vmro.GetVolRestoreVolumeName(vr)))
}

func (re *ResticRestoreEngine) checkJobName(vmr *harvesterv1.VirtualMachineRestore, vr *harvesterv1.VolumeRestore) string {
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", checkJobNamePrefix, re.vmro.GetName(vmr), re.vmro.GetVolRestoreVolumeName(vr)))
}

// jobLocator returns the name and namespace to look up the restore Job for vr.
// UpdateProgress is called with only a VolumeRestore handle but the job name
// embeds the VMRestore name; we recover that from the destination PVC's
// restore-name annotation (set in createPVC via BuildRestoreAnnotations).
func (re *ResticRestoreEngine) jobLocator(vr *harvesterv1.VolumeRestore) (name, namespace string, ok bool) {
	volName := re.vmro.GetVolRestoreVolumeName(vr)
	ns := re.vmro.GetVolRestorePVCNamespace(vr)
	if volName == "" || ns == "" {
		return "", "", false
	}
	pvc, err := re.pvcCache.Get(ns, re.vmro.GetVolRestorePVCName(vr))
	if err != nil {
		return "", "", false
	}
	restoreName := pvc.Annotations[restorecommon.RestoreNameAnnotation]
	if restoreName == "" {
		return "", "", false
	}
	return strings.ToLower(fmt.Sprintf("%s-%s-%s", jobNamePrefix, restoreName, volName)), ns, true
}
