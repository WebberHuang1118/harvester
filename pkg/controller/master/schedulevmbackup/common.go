package schedulevmbackup

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.uber.org/multierr"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/controller/master/backup"
	"github.com/harvester/harvester/pkg/ref"
	"github.com/harvester/harvester/pkg/util"
)

const (
	scheduleVMBackupKindName = "ScheduleVMBackup"
	timeFormat               = "20060102.1504"

	reachMaxFailure = "Reach Max Failure"
)

const (
	updateInterval    = 5 * time.Second
	reconcileInterval = 5 * time.Second

	svmbackupPrefix = "svmb"

	cronJobNamespace    = "harvester-system"
	cronJobBackoffLimit = 3
	cronJobCmd          = "sleep"
	cronJobArg          = "10"
)

var scheduleVMBackupKind = harvesterv1.SchemeGroupVersion.WithKind(scheduleVMBackupKindName)

func cronJobName(svmbackup *harvesterv1.ScheduleVMBackup) string {
	return fmt.Sprintf("%s-%s", svmbackupPrefix, svmbackup.UID)
}

func getCronJob(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup) (*batchv1.CronJob, error) {
	cronJob, err := h.cronJobCache.Get(cronJobNamespace, cronJobName(svmbackup))
	if err != nil {
		return nil, err
	}

	return cronJob, nil
}

func vmBackupName(svmbackup *harvesterv1.ScheduleVMBackup, timestamp string) string {
	return fmt.Sprintf("%s-%s-%s", svmbackupPrefix, svmbackup.UID, timestamp)
}

func getVMBackup(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup, timestamp string) (*harvesterv1.VirtualMachineBackup, error) {
	vmBackup, err := h.vmBackupCache.Get(svmbackup.Namespace, vmBackupName(svmbackup, timestamp))
	if err != nil {
		return nil, err
	}

	return vmBackup, nil
}

func resolveSVMBackupRef(h *svmbackupHandler, obj metav1.Object) *harvesterv1.ScheduleVMBackup {
	var annotations = obj.GetAnnotations()
	if annotations == nil || annotations[util.AnnotationSVMBackupID] == "" {
		return nil
	}

	namespace, name := ref.Parse(annotations[util.AnnotationSVMBackupID])
	svmbackup, err := h.svmbackupCache.Get(namespace, name)
	if err != nil {
		return nil
	}

	return svmbackup
}

func currentVMBackups(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup) (
	[]*harvesterv1.VirtualMachineBackup, []*harvesterv1.VirtualMachineBackup,
	*harvesterv1.VirtualMachineBackup, int, error) {
	sets := labels.Set{
		util.LabelSVMBackupUID: string(svmbackup.UID),
	}
	vmbackups, err := h.vmBackupCache.List(svmbackup.Namespace, sets.AsSelector())
	if err != nil {
		return nil, nil, nil, 0, err
	}

	sort.Slice(vmbackups, func(i, j int) bool {
		time1, _ := time.Parse(timeFormat, vmbackups[i].Labels[util.LabelSVMBackupTimestamp])
		time2, _ := time.Parse(timeFormat, vmbackups[j].Labels[util.LabelSVMBackupTimestamp])
		return time1.Before(time2)
	})

	errVMBackups := []*harvesterv1.VirtualMachineBackup{}
	var lastVMBackup *harvesterv1.VirtualMachineBackup
	var failure int

	for _, vb := range vmbackups {
		lastVMBackup = vb

		if vb.Status == nil {
			continue
		}

		if vb.Status.Error != nil {
			errVMBackups = append(errVMBackups, vb)
			failure++
		}

		if vb.Status.ReadyToUse != nil && *vb.Status.ReadyToUse {
			failure = 0
		}
	}

	return vmbackups, errVMBackups, lastVMBackup, failure, nil
}

func createVMBackup(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup, timestamp string) (*harvesterv1.VirtualMachineBackup, error) {
	vmBackup := &harvesterv1.VirtualMachineBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmBackupName(svmbackup, timestamp),
			Namespace: svmbackup.Namespace,
			Annotations: map[string]string{
				util.AnnotationSVMBackupID: ref.Construct(svmbackup.Namespace, svmbackup.Name),
			},
			Labels: map[string]string{
				util.LabelSVMBackupUID:       string(svmbackup.UID),
				util.LabelSVMBackupTimestamp: timestamp,
			},
		},
		Spec: svmbackup.Spec.VMBackupSpec,
	}

	return h.vmBackupClient.Create(vmBackup)
}

// Try our best to delete outdated VM Backup and LH snapshots
// even error occurs during the process, log the error and continue to traverse all resources
func cleanseVMBackup(h *svmbackupHandler, vmbackup *harvesterv1.VirtualMachineBackup) error {
	var errs error
	for _, vb := range vmbackup.Status.VolumeBackups {
		snapshot, err := h.snapshotCache.Get(vmbackup.Namespace, *vb.Name)
		if err != nil {
			if !errors.IsNotFound(err) {
				errs = multierr.Append(errs, fmt.Errorf("get snapshot %v/%v failed %v",
					vmbackup.Namespace, *vb.Name, err))
			}
			continue
		}

		if snapshot.Status == nil || snapshot.Status.BoundVolumeSnapshotContentName == nil {
			errs = multierr.Append(errs, fmt.Errorf("volumesnapshot %v not bound to snapshotcontent", snapshot.Name))
			continue
		}

		lhsnapshotName := strings.Replace(*snapshot.Status.BoundVolumeSnapshotContentName, "snapcontent", "snapshot", 1)
		lhsnapshot, err := h.lhsnapshotCache.Get(util.LonghornSystemNamespaceName, lhsnapshotName)
		if err != nil {
			errs = multierr.Append(errs, fmt.Errorf("get lhsnapshot %v failed %v", lhsnapshotName, err))
			continue
		}

		propagation := metav1.DeletePropagationForeground
		if err := h.lhsnapshotClient.Delete(util.LonghornSystemNamespaceName, lhsnapshot.Name,
			&metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("delete lhsnapshot %v failed %v", lhsnapshotName, err))
			continue
		}
	}

	propagation := metav1.DeletePropagationForeground
	if err := h.vmBackupClient.Delete(vmbackup.Namespace, vmbackup.Name,
		&metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
		errs = multierr.Append(errs, fmt.Errorf("delete vmbackup %v/%v fail %v", vmbackup.Namespace, vmbackup.Name, err))
	}
	return errs
}

func clearVMBackups(h *svmbackupHandler, vmbackups []*harvesterv1.VirtualMachineBackup,
	target int, oldCleared map[string]bool) (int, map[string]bool, error) {
	newCleared := map[string]bool{}
	var errs error

	for k, v := range oldCleared {
		newCleared[k] = v
	}

	left := target
	for i := 0; i < len(vmbackups); i++ {
		if left <= 0 {
			break
		}

		if find := newCleared[vmbackups[i].Name]; find {
			continue
		}

		left--
		newCleared[vmbackups[i].Name] = true

		err := cleanseVMBackup(h, vmbackups[i])
		if err == nil {
			continue
		}

		errs = multierr.Append(errs, err)
	}

	return left, newCleared, errs
}

func gcVMBackups(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup) error {
	vmBackups, errVMBackups, lastVMBackup, _, err := currentVMBackups(h, svmbackup)
	if err != nil {
		return err
	}

	if lastVMBackup == nil {
		return nil
	}

	if backup.IsBackupProgressing(lastVMBackup) {
		h.svmbackupController.EnqueueAfter(svmbackup.Namespace, svmbackup.Name, updateInterval)
		return nil
	}

	if backup.GetVMBackupError(lastVMBackup) != nil {
		return nil
	}

	// we clear the failure backups first, and the successful backup from the oldest one
	// the #target-delete-backups according to `.spec.retain`
	var errs error
	left, cleared, err := clearVMBackups(h, errVMBackups, len(vmBackups)-svmbackup.Spec.Retain, nil)
	if err != nil {
		multierr.Append(errs, fmt.Errorf("svmbackup %s clear failure VMBackups failed %w", svmbackup.Name, err))
	}

	if left <= 0 {
		return errs
	}

	left, _, err = clearVMBackups(h, vmBackups, left, cleared)
	if err != nil {
		multierr.Append(errs, fmt.Errorf("svmbackup %s clear complete VMBackups failed %w", svmbackup.Name, err))
	}

	if left > 0 {
		multierr.Append(errs, fmt.Errorf("svmbackup %s unable to gc %d VMBackups", svmbackup.Name, left))
	}

	return errs
}

// Record VM backup status and volume backups status in `.staus.vmbackupInfo`
func convertVMBackupToInfo(vmbackup *harvesterv1.VirtualMachineBackup) harvesterv1.VMBackupInfo {
	var vmBackupInfo harvesterv1.VMBackupInfo

	vmBackupInfo.Name = vmbackup.Name
	if vmbackup.Status == nil {
		return vmBackupInfo
	}

	status := vmbackup.Status
	if status.ReadyToUse != nil {
		vmBackupInfo.ReadyToUse = status.ReadyToUse
	}

	if status.Error != nil {
		vmBackupInfo.Error = status.Error
	}

	if len(status.VolumeBackups) == 0 {
		return vmBackupInfo
	}

	vmBackupInfo.VolumeBackupInfo = make([]harvesterv1.VolumeBackupInfo, len(status.VolumeBackups))
	for i := 0; i < len(status.VolumeBackups); i++ {
		vb := status.VolumeBackups[i]
		if vb.Name != nil {
			vmBackupInfo.VolumeBackupInfo[i].Name = vb.Name
		}

		if vb.ReadyToUse != nil {
			vmBackupInfo.VolumeBackupInfo[i].ReadyToUse = vb.ReadyToUse
		}

		if vb.Error != nil {
			vmBackupInfo.VolumeBackupInfo[i].Error = vb.Error
		}
	}

	return vmBackupInfo
}

func reconcileVMBackupList(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup) error {
	vmbackups, _, _, failure, err := currentVMBackups(h, svmbackup)
	if err != nil {
		return err
	}

	svmbackupCpy := svmbackup.DeepCopy()
	svmbackupCpy.Status.VMBackupInfo = make([]harvesterv1.VMBackupInfo, len(vmbackups))
	svmbackupCpy.Status.Failure = failure
	for i := 0; i < len(vmbackups); i++ {
		svmbackupCpy.Status.VMBackupInfo[i] = convertVMBackupToInfo(vmbackups[i])
	}

	if reflect.DeepEqual(svmbackup.Status, svmbackupCpy.Status) {
		return nil
	}

	if _, err := h.svmbackupClient.Update(svmbackupCpy); err != nil {
		return err
	}

	return nil
}

func updateVMBackups(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup) error {
	var errs error
	err := gcVMBackups(h, svmbackup)
	if err != nil {
		errs = multierr.Append(errs, err)
	}

	err = reconcileVMBackupList(h, svmbackup)
	if err != nil {
		errs = multierr.Append(errs, err)
	}

	return errs
}

func updateSuspendStatus(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup, suspend bool, msg string) error {
	cronJob, err := getCronJob(h, svmbackup)
	if err != nil {
		return err
	}

	cronJobCpy := cronJob.DeepCopy()
	cronJobCpy.Spec.Suspend = &suspend
	if !reflect.DeepEqual(cronJob, cronJobCpy) {
		if _, err := h.cronJobsClient.Update(cronJobCpy); err != nil {
			return err
		}
	}

	svmbackupCpy := svmbackup.DeepCopy()
	if suspend {
		svmbackupCpy.Status.Suspend = true
		harvesterv1.BackupSuspend.True(svmbackupCpy)
		harvesterv1.BackupSuspend.Reason(svmbackupCpy, reachMaxFailure)
		harvesterv1.BackupSuspend.Message(svmbackupCpy, msg)
	} else {
		svmbackupCpy.Status.Suspend = false
		svmbackupCpy.Spec.ResumeRequest = false
		harvesterv1.BackupSuspend.False(svmbackupCpy)
		harvesterv1.BackupSuspend.Reason(svmbackupCpy, "")
		harvesterv1.BackupSuspend.Message(svmbackupCpy, "")
	}

	if reflect.DeepEqual(svmbackup, svmbackupCpy) {
		return nil
	}

	if _, err := h.svmbackupClient.Update(svmbackupCpy); err != nil {
		return err
	}

	return nil
}

func handleReachMaxFailure(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup, msg string) error {
	return updateSuspendStatus(h, svmbackup, true, msg)
}

func handleResume(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup) error {
	_, errVMbackups, _, failure, err := currentVMBackups(h, svmbackup)
	if err != nil {
		return err
	}

	if failure < svmbackup.Spec.MaxFailure {
		return updateSuspendStatus(h, svmbackup, false, "")
	}

	// Remove all failure backups for resuming schedule
	for _, vmbackup := range errVMbackups {
		if err := cleanseVMBackup(h, vmbackup); err != nil {
			return err
		}
	}

	return fmt.Errorf("svmbackup %s/%s retry handle resume", svmbackup.Namespace, svmbackup.Name)
}

func newVMBackups(h *svmbackupHandler, svmbackup *harvesterv1.ScheduleVMBackup, timestamp string) (*harvesterv1.VirtualMachineBackup, error) {
	oldVMBackups, _, lastVMBackup, failure, err := currentVMBackups(h, svmbackup)
	if err != nil {
		return nil, err
	}

	if len(oldVMBackups) == 0 {
		vmbackup, err := createVMBackup(h, svmbackup, timestamp)
		if err != nil {
			return nil, err
		}

		return vmbackup, nil
	}

	if failure >= svmbackup.Spec.MaxFailure {
		msg := fmt.Sprintf("failure backups %v reach max tolerance %v", failure, svmbackup.Spec.MaxFailure)
		return nil, handleReachMaxFailure(h, svmbackup, msg)
	}

	if backup.IsBackupProgressing(lastVMBackup) {
		return nil, fmt.Errorf("lastest vm backup %v/%v in progress", lastVMBackup.Namespace, lastVMBackup.Name)
	}

	vmbackup, err := createVMBackup(h, svmbackup, timestamp)
	if err != nil {
		return nil, err
	}

	return vmbackup, nil
}
