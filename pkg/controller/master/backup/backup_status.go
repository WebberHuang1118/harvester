package backup

import (
	"fmt"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	lhv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
)

func (h *Handler) updateConditions(vmb *harvesterv1.VirtualMachineBackup) error {
	var vmbCpy = vmb.DeepCopy()
	if h.vmbo.IsProcessing(vmbCpy) {
		vmbCpy = h.vmbo.UpdateProcessingNoCommit(vmbCpy)
	}

	ready := true
	var volBackupErr error
	var volumeSizeSum int64
	var progressWeightSum int64

	for i := range h.vmbo.GetVolBackups(vmbCpy) {
		vb := h.vmbo.GetVolBackup(vmbCpy, i)
		if !h.vmbo.GetVolBackupReadyToUse(vb) {
			ready = false
		}

		volumeSize, err := h.engines[h.vmbo.GetType(vmbCpy)].UpdateProgress(vb)
		if err != nil {
			return err
		}

		volumeSizeSum += volumeSize
		progressWeightSum += int64(h.vmbo.GetVolBackupProgress(vb)) * volumeSizeSum

		if h.vmbo.GetVolBackupError(vb) != nil {
			volBackupErr = fmt.Errorf("VolumeSnapshot %s in error state", *h.vmbo.GetVolBackupName(vb))
			break
		}
	}

	if volumeSizeSum != 0 {
		if err := h.vmbo.SetProgress(vmbCpy, int(progressWeightSum/volumeSizeSum)); err != nil {
			return err
		}
	}

	if ready && !h.vmbo.IsReady(vmbCpy) {
		vmbCpy = h.vmbo.UpdateCompleteNoCommit(vmbCpy)
	}

	if volBackupErr != nil && !h.vmbo.IsSyncErrMsg(vmbCpy, volBackupErr.Error()) {
		vmbCpy = h.vmbo.UpdateErrorNoCommit(vmbCpy, volBackupErr)
	}

	if err := h.vmbo.SetReadyToUse(vmbCpy, ready); err != nil {
		return err
	}
	_, err := h.vmbo.Update(vmb, vmbCpy)
	return err
}

func (h *Handler) updateVolumeSnapshotChanged(_ string, snapshot *snapshotv1.VolumeSnapshot) (*snapshotv1.VolumeSnapshot, error) {
	if snapshot == nil || snapshot.DeletionTimestamp != nil {
		return nil, nil
	}

	controllerRef := metav1.GetControllerOf(snapshot)

	// If it has a ControllerRef, that's all that matters.
	if controllerRef != nil {
		ref := h.resolveVolSnapshotRef(snapshot.Namespace, controllerRef)
		if ref == nil {
			return nil, nil
		}
		h.vmBackupController.Enqueue(ref.Namespace, ref.Name)
	}
	return nil, nil
}

// resolveVolSnapshotRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (h *Handler) resolveVolSnapshotRef(namespace string, controllerRef *metav1.OwnerReference) *harvesterv1.VirtualMachineBackup {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != vmBackupKind.Kind {
		return nil
	}
	vmb, err := h.vmBackupCache.Get(namespace, controllerRef.Name)
	if err != nil {
		return nil
	}
	if h.vmbo.GetUID(vmb) != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return vmb
}

func (h *Handler) OnLHBackupChanged(_ string, lhBackup *lhv1beta2.Backup) (*lhv1beta2.Backup, error) {
	if lhBackup == nil || lhBackup.DeletionTimestamp != nil || lhBackup.Status.SnapshotName == "" {
		return nil, nil
	}

	vmb, err := h.getVMBackupFromLHBackup(lhBackup)
	if err != nil || vmb == nil {
		return nil, err
	}

	if h.vmbo.GetBackupTarget(vmb) == nil {
		return nil, nil
	}

	if err := h.updateVolumeBackupLHNames(vmb, lhBackup.Name); err != nil {
		return nil, err
	}

	// Enqueue to trigger progress update in updateConditions()
	h.vmBackupController.Enqueue(h.vmbo.GetNamespace(vmb), h.vmbo.GetName(vmb))
	return nil, nil
}

// getVMBackupFromLHBackup retrieves the VirtualMachineBackup associated with a Longhorn backup
func (h *Handler) getVMBackupFromLHBackup(lhBackup *lhv1beta2.Backup) (*harvesterv1.VirtualMachineBackup, error) {
	vsContent, err := h.snapshotContentCache.Get(strings.Replace(lhBackup.Status.SnapshotName, "snapshot", "snapcontent", 1))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	vs, err := h.snapshotCache.Get(vsContent.Spec.VolumeSnapshotRef.Namespace, vsContent.Spec.VolumeSnapshotRef.Name)
	if err != nil {
		return nil, err
	}

	controllerRef := metav1.GetControllerOf(vs)
	if controllerRef == nil {
		return nil, nil
	}

	return h.resolveVolSnapshotRef(vs.Namespace, controllerRef), nil
}

// updateVolumeBackupLHNames updates the Longhorn backup names for all volume backups
func (h *Handler) updateVolumeBackupLHNames(vmb *harvesterv1.VirtualMachineBackup, lhBackupName string) error {
	vmbCpy := vmb.DeepCopy()
	volumeBackups := h.vmbo.GetVolBackups(vmbCpy)

	for volumeIndex := range volumeBackups {
		volumeBackup := h.vmbo.GetVolBackup(vmbCpy, volumeIndex)
		if h.vmbo.GetVolBackupName(volumeBackup) != nil {
			if err := h.vmbo.SetVolBackupLHBackupName(volumeBackup, lhBackupName); err != nil {
				return fmt.Errorf("failed to set volume backup LH backup name: %w", err)
			}
		}
	}

	_, err := h.vmbo.UpdateByStatus(vmb, vmbCpy)
	return err
}
