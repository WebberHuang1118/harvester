package longhorn

import (
	"context"
	"fmt"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	"github.com/longhorn/backupstore"
	lhv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/storage/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/backup/common"
	"github.com/harvester/harvester/pkg/backup/engine"
	ctllonghornv2 "github.com/harvester/harvester/pkg/generated/controllers/longhorn.io/v1beta2"
	ctlsnapshotv1 "github.com/harvester/harvester/pkg/generated/controllers/snapshot.storage.k8s.io/v1"
	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
	backuputil "github.com/harvester/harvester/pkg/util/backup"
)

const (
	longhornDriver         = "driver.longhorn.io"
	longhornBackupScheme   = "bak://"
	backupProgressComplete = 100
)

type LonghornEngine struct {
	vmbo          common.VMBackupOperator
	vsHelper      *common.VolumeSnapshotHelper
	pvcCache      ctlcorev1.PersistentVolumeClaimCache
	scCache       ctlstoragev1.StorageClassCache
	lhbackupCache ctllonghornv2.BackupCache
}

func GetBackupEngine(
	vmbo common.VMBackupOperator,
	vsCache ctlsnapshotv1.VolumeSnapshotCache,
	vsClient ctlsnapshotv1.VolumeSnapshotClient,
	vsContentCache ctlsnapshotv1.VolumeSnapshotContentCache,
	vsContentClient ctlsnapshotv1.VolumeSnapshotContentClient,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	scCache ctlstoragev1.StorageClassCache,
	lhbackupCache ctllonghornv2.BackupCache,
) engine.BackupEngine {
	return &LonghornEngine{
		vmbo:          vmbo,
		vsHelper:      common.NewVolumeSnapshotHelper(vsCache, vsClient, vsContentCache, vsContentClient, vmbo, pvcCache, scCache),
		pvcCache:      pvcCache,
		scCache:       scCache,
		lhbackupCache: lhbackupCache,
	}
}

// vsContentName generates a VolumeSnapshotContent name from a VolumeBackup
func (le *LonghornEngine) vsContentName(vb *harvesterv1.VolumeBackup) string {
	volBackupName := le.vmbo.GetVolBackupName(vb)
	if volBackupName == nil {
		return ""
	}
	return fmt.Sprintf("%s-vsc", *volBackupName)
}

// getExistingVSContent retrieves existing VolumeSnapshotContent or returns nil if not found
func (le *LonghornEngine) getExistingVSContent(vsContentName string) (*snapshotv1.VolumeSnapshotContent, error) {
	vsContent, err := le.vsHelper.GetVolumeSnapshotContent(vsContentName)
	if err == nil {
		return vsContent, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get VolumeSnapshotContent %s: %w", vsContentName, err)
	}
	return nil, nil
}

// getVolumeBackupNameOrDefault returns the volume backup name or a default string for logging
func (le *LonghornEngine) getVolumeBackupNameOrDefault(vb *harvesterv1.VolumeBackup) string {
	volBackupName := le.vmbo.GetVolBackupName(vb)
	if volBackupName == nil {
		return "<nil>"
	}
	return *volBackupName
}

// buildSnapshotHandle constructs the Longhorn backup snapshot handle
// Ref: https://longhorn.io/docs/1.2.3/snapshots-and-backups/csi-snapshot-support/restore-a-backup-via-csi/#restore-a-backup-that-has-no-associated-volumesnapshot
func (le *LonghornEngine) buildSnapshotHandle(volumeName, backupName string) string {
	return fmt.Sprintf("%s%s/%s", longhornBackupScheme, volumeName, backupName)
}

// validateAndGetLonghornBackup validates the volume backup and retrieves the Longhorn backup
func (le *LonghornEngine) validateAndGetLonghornBackup(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
) (string, error) {
	if le.vmbo.GetVolBackupLHBackupName(vb) == nil {
		return "", fmt.Errorf("lh backup name is nil for volume backup %s in VMBackup %s/%s",
			le.getVolumeBackupNameOrDefault(vb), le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb))
	}

	lhBackup, err := le.lhbackupCache.Get(util.LonghornSystemNamespaceName, *le.vmbo.GetVolBackupLHBackupName(vb))
	if err != nil {
		return "", fmt.Errorf("failed to get Longhorn backup %s: %w", *le.vmbo.GetVolBackupLHBackupName(vb), err)
	}

	if lhBackup.Status.VolumeName == "" {
		return "", fmt.Errorf("lh backup %s has empty volumeName for vmbackup %s/%s volume %s",
			lhBackup.Name, le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb), le.getVolumeBackupNameOrDefault(vb))
	}

	snapshotHandle := le.buildSnapshotHandle(lhBackup.Status.VolumeName, lhBackup.Name)
	return snapshotHandle, nil
}

// buildVolumeSnapshotContent constructs a VolumeSnapshotContent object
func (le *LonghornEngine) buildVolumeSnapshotContent(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vsc *snapshotv1.VolumeSnapshotClass,
	vscName string,
	snapshotHandle string,
) *snapshotv1.VolumeSnapshotContent {
	volBackupName := le.vmbo.GetVolBackupName(vb)

	return &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            vscName,
			OwnerReferences: []metav1.OwnerReference{le.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: snapshotv1.VolumeSnapshotContentSpec{
			Driver:         longhornDriver,
			DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete,
			Source: snapshotv1.VolumeSnapshotContentSource{
				SnapshotHandle: ptr.To(snapshotHandle),
			},
			VolumeSnapshotClassName: ptr.To(vsc.Name),
			VolumeSnapshotRef: corev1.ObjectReference{
				Name:      *volBackupName,
				Namespace: le.vmbo.GetNamespace(vmb),
			},
		},
	}
}

func (le *LonghornEngine) createVolumeSnapshotContent(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vsc *snapshotv1.VolumeSnapshotClass,
) (*snapshotv1.VolumeSnapshotContent, error) {
	vsContentName := le.vsContentName(vb)
	if vsContentName == "" {
		return nil, fmt.Errorf("volume backup name is nil for VMBackup %s/%s",
			le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb))
	}

	logrus.Debugf("attempting to create VolumeSnapshotContent %s", vsContentName)

	// Check if VolumeSnapshotContent already exists
	existingContent, err := le.getExistingVSContent(vsContentName)
	if err != nil {
		return nil, err
	}
	if existingContent != nil {
		logrus.Debugf("VolumeSnapshotContent %s already exists, reusing", vsContentName)
		return existingContent, nil
	}

	// Validate volume backup and get snapshot handle
	snapshotHandle, err := le.validateAndGetLonghornBackup(vmb, vb)
	if err != nil {
		return nil, err
	}

	// Validate volume backup name for VolumeSnapshotRef
	volBackupName := le.vmbo.GetVolBackupName(vb)
	if volBackupName == nil {
		return nil, fmt.Errorf("volume backup name is nil for VMBackup %s/%s",
			le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb))
	}

	fields := le.getLogFields(vmb, vb)
	fields["name"] = vsContentName
	fields["snapshotHandle"] = snapshotHandle
	logrus.WithFields(fields).Info("creating VolumeSnapshotContent")

	// Build and create VolumeSnapshotContent
	vsContent := le.buildVolumeSnapshotContent(vmb, vb, vsc, vsContentName, snapshotHandle)
	return le.vsHelper.CreateVolumeSnapshotContent(vsContent)
}

func (le *LonghornEngine) createVSFromLHBackup(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vsc *snapshotv1.VolumeSnapshotClass) (*snapshotv1.VolumeSnapshot, error) {

	vsContent, err := le.createVolumeSnapshotContent(vmb, vb, vsc)
	if err != nil {
		return nil, fmt.Errorf("failed to create VolumeSnapshotContent: %w", err)
	}

	vs := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            *le.vmbo.GetVolBackupName(vb),
			Namespace:       le.vmbo.GetNamespace(vmb),
			OwnerReferences: []metav1.OwnerReference{le.vsHelper.BuildOwnerReference(vmb)},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				VolumeSnapshotContentName: &vsContent.Name,
			},
			VolumeSnapshotClassName: ptr.To(vsc.Name),
		},
	}

	logrus.WithFields(le.getLogFields(vmb, vb)).Info("creating VolumeSnapshot from Longhorn backup")
	return le.vsHelper.CreateVolumeSnapshot(vs)
}

func (le *LonghornEngine) createVSFromPVC(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vsc *snapshotv1.VolumeSnapshotClass) (*snapshotv1.VolumeSnapshot, error) {

	return le.vsHelper.CreateVolumeSnapshotFromPVC(vmb, vb, vsc, le.vsHelper.BuildOwnerReference(vmb))
}

func (le *LonghornEngine) getLogFields(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) logrus.Fields {
	fields := le.vsHelper.GetLogFields(vmb, vb)
	if vb != nil {
		if lhName := le.vmbo.GetVolBackupLHBackupName(vb); lhName != nil {
			fields["longhornBackup"] = *lhName
		}
	}
	return fields
}

func (le *LonghornEngine) checkLHBackup(name string) (string, error) {
	lb, err := le.lhbackupCache.Get(util.LonghornSystemNamespaceName, name)
	if err != nil {
		return "", err
	}

	if lb.Status.State != lhv1beta2.BackupStateCompleted {
		return fmt.Sprintf("backup %s is not completed", name), nil
	}

	if lb.DeletionTimestamp != nil {
		return fmt.Sprintf("backup %s is being deleted", name), nil
	}
	return "", nil
}

func (le *LonghornEngine) checkVolInBackupTarget(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup, t *settings.BackupTarget) (string, error) {
	volName := le.vmbo.GetVolBackupVolumeName(vb)
	vols, err := backupstore.List(volName, backuputil.ConstructEndpoint(t), false)
	if err != nil {
		// The backup target may be offline. In this case, we don't want to trigger reconciliation.
		return err.Error(), nil
	}

	handleMissing := func(msg string) (string, error) {
		logrus.WithFields(le.getLogFields(vmb, vb)).Warn(msg + ", change the VMBackup to not ready")
		if err := le.vmbo.SetVolBackupReadyToUse(vb, ptr.To(false)); err != nil {
			return "", fmt.Errorf("failed to set volume backup ready to use: %w", err)
		}
		return msg, nil
	}

	if vols[volName] == nil {
		return handleMissing(fmt.Sprintf("cannot find volume %s in the backup target", volName))
	}

	lhBackupName := *le.vmbo.GetVolBackupLHBackupName(vb)
	if vols[volName].Backups[lhBackupName] == nil {
		return handleMissing(fmt.Sprintf("cannot find longhorn backup %s in the backup target", lhBackupName))
	}
	return "", nil
}

func (le *LonghornEngine) shouldSkipVSUpdate(vmb *harvesterv1.VirtualMachineBackup, vb *harvesterv1.VolumeBackup) (bool, error) {
	if !le.vmbo.IsTransitToNonReady(vmb) {
		return false, nil
	}
	lhBackupName := le.vmbo.GetVolBackupLHBackupName(vb)
	if lhBackupName == nil {
		return false, nil
	}

	msg, err := le.checkLHBackup(*lhBackupName)
	if apierrors.IsNotFound(err) {
		// Don't return not found error here, because it changes the VMBackup status message and there will not have "Change back to non-ready" message.
		// In the next reconcile, the shouldSkipVolumeSnapshotUpdate will return false, so the volume backup will get updated from VolumeSnapshot.
		return true, nil
	}

	if err != nil {
		logrus.WithError(err).WithFields(le.getLogFields(vmb, vb)).Warn("cannot check longhorn backup")
		return true, err
	}

	if msg != "" {
		logrus.WithFields(le.getLogFields(vmb, vb)).WithField(logrus.ErrorKey, msg).Infof("longhorn backup is not ready")
		return true, nil
	}

	tValue := settings.BackupTargetSet.Get()
	t, err := settings.DecodeBackupTarget(tValue)
	if err != nil {
		logrus.WithError(err).WithFields(le.getLogFields(vmb, vb)).Warnf("failed to decode backup target %s", tValue)
		return true, err
	}

	msg, err = le.checkVolInBackupTarget(vmb, vb, t)
	if err != nil {
		logrus.WithError(err).WithFields(le.getLogFields(vmb, vb)).Warnf("failed to check volume in backup target")
		return true, err
	}

	if msg != "" {
		logrus.WithFields(le.getLogFields(vmb, vb)).WithField(logrus.ErrorKey, msg).Infof("volume is not in backup target")
		return true, nil
	}
	return false, nil
}

func (le *LonghornEngine) Create(
	vmb *harvesterv1.VirtualMachineBackup,
	volIndex int,
	vscMap map[string]snapshotv1.VolumeSnapshotClass,
) error {
	logrus.Infof("LonghornEngine Create called for VMBackup %s/%s volume index %d",
		le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb), volIndex)

	vb := le.vmbo.GetVolBackup(vmb, volIndex)
	volBackupName := le.vmbo.GetVolBackupName(vb)
	if volBackupName == nil {
		return fmt.Errorf("volume backup name is nil for VMBackup %s/%s at index %d",
			le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb), volIndex)
	}

	vsName := *volBackupName
	vs, err := le.vsHelper.GetVolumeSnapshot(le.vmbo.GetNamespace(vmb), vsName)
	if err != nil {
		return err
	}

	if err := le.vsHelper.CheckSnapshotDeletionStatus(vs, vmb, vb); err != nil {
		return err
	}

	if vs == nil {
		vs, err = le.ensureVolumeSnapshotExists(vmb, vb, vscMap)
		if err != nil {
			return err
		}
	}

	return le.updateVolumeBackupFromSnapshot(vmb, vb, vs)
}

// ensureVolumeSnapshotExists creates a new volume snapshot if it doesn't exist
func (le *LonghornEngine) ensureVolumeSnapshotExists(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vscMap map[string]snapshotv1.VolumeSnapshotClass,
) (*snapshotv1.VolumeSnapshot, error) {
	if err := le.vsHelper.TryFreezeFS(context.Background(), vmb); err != nil {
		return nil, err
	}

	csiDriver := le.vmbo.GetVolBackupCSIDriver(vb)
	vsc, exists := vscMap[csiDriver]
	if !exists {
		return nil, fmt.Errorf("VolumeSnapshotClass not found for CSI driver %s", csiDriver)
	}

	if le.vmbo.GetVolBackupLHBackupName(vb) != nil {
		return le.createVSFromLHBackup(vmb, vb, &vsc)
	}
	return le.createVSFromPVC(vmb, vb, &vsc)

}

// updateVolumeBackupFromSnapshot updates the volume backup status from the snapshot,
// checking if the update should be skipped first
func (le *LonghornEngine) updateVolumeBackupFromSnapshot(
	vmb *harvesterv1.VirtualMachineBackup,
	vb *harvesterv1.VolumeBackup,
	vs *snapshotv1.VolumeSnapshot,
) error {
	skip, err := le.shouldSkipVSUpdate(vmb, vb)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	return le.vsHelper.UpdateVolumeBackupStatus(vb, vs)
}

func (le *LonghornEngine) UpdateProgress(vb *harvesterv1.VolumeBackup) (int64, error) {
	if le.vmbo.GetVolBackupReadyToUse(vb) {
		vb.Progress = backupProgressComplete
		return backupProgressComplete, nil
	}

	if le.vmbo.GetVolBackupLHBackupName(vb) == nil {
		return 0, nil
	}

	lhBackup, err := le.lhbackupCache.Get(util.LonghornSystemNamespaceName, *le.vmbo.GetVolBackupLHBackupName(vb))
	if err != nil {
		return 0, err
	}

	vb.Progress = lhBackup.Status.Progress
	return int64(vb.Progress), nil
}

// ForceDelete removes VolumeSnapshot and VolumeSnapshotContent with their finalizers
// This is necessary when the backup target changes and resources need immediate cleanup
// to prevent issues with Longhorn backup deletion
func (le *LonghornEngine) ForceDelete(vmb *harvesterv1.VirtualMachineBackup, volIndex int) error {
	vb := le.vmbo.GetVolBackup(vmb, volIndex)
	volBackupName := le.vmbo.GetVolBackupName(vb)
	if volBackupName == nil {
		return fmt.Errorf("volume backup name is nil for VMBackup %s/%s at index %d",
			le.vmbo.GetNamespace(vmb), le.vmbo.GetName(vmb), volIndex)
	}

	vsName := *volBackupName
	namespace := le.vmbo.GetNamespace(vmb)

	vs, err := le.vsHelper.GetVolumeSnapshot(namespace, vsName)
	if err != nil {
		return err
	}

	if vs == nil {
		return nil
	}

	// Remove finalizers and delete VolumeSnapshot
	if err := le.vsHelper.ForceDeleteVolumeSnapshot(namespace, vsName); err != nil {
		return fmt.Errorf("failed to force delete VolumeSnapshot: %w", err)
	}

	// Remove finalizers and delete VolumeSnapshotContent if it exists
	if vs.Status == nil || vs.Status.BoundVolumeSnapshotContentName == nil {
		return nil
	}

	if err := le.vsHelper.ForceDeleteVolumeSnapshotContent(*vs.Status.BoundVolumeSnapshotContentName); err != nil {
		return fmt.Errorf("failed to force delete VolumeSnapshotContent: %w", err)
	}

	return nil
}
