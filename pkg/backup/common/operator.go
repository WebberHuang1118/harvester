package common

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
	ctlkubevirtv1 "github.com/harvester/harvester/pkg/generated/controllers/kubevirt.io/v1"
	ctlsnapshotv1 "github.com/harvester/harvester/pkg/generated/controllers/snapshot.storage.k8s.io/v1"
	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
)

const (
	backupTargetAnnotation       = "backup.harvesterhci.io/backup-target"
	backupBucketNameAnnotation   = "backup.harvesterhci.io/bucket-name"
	backupBucketRegionAnnotation = "backup.harvesterhci.io/bucket-region"
	volumeBackupNameFormat       = "%s-volume-%s"
	defaultFreezeDuration        = 1 * time.Second
	changeToNonReadyMessage      = "Change back to non-ready"
)

type VMBackupOperator interface {
	GetVolBackups(vmb *harvesterv1.VirtualMachineBackup) []harvesterv1.VolumeBackup
	GetVolBackup(vmb *harvesterv1.VirtualMachineBackup, index int) *harvesterv1.VolumeBackup
	GetSanitizeVolBackups(vmb *harvesterv1.VirtualMachineBackup) []harvesterv1.VolumeBackup
	GetVolBackupName(vb *harvesterv1.VolumeBackup) *string
	GetVolBackupPVCNameSpace(vb *harvesterv1.VolumeBackup) string
	GetVolBackupPVCName(vb *harvesterv1.VolumeBackup) string
	GetVolBackupVolumeName(vb *harvesterv1.VolumeBackup) string
	GetVolBackupCSIDriver(vb *harvesterv1.VolumeBackup) string
	GetVolBackupReadyToUse(vb *harvesterv1.VolumeBackup) bool
	GetVolBackupSize(vb *harvesterv1.VolumeBackup) int64
	GetVolBackupProgress(vb *harvesterv1.VolumeBackup) int
	GetVolBackupLHBackupName(vb *harvesterv1.VolumeBackup) *string
	GetVolBackupSCName(vb *harvesterv1.VolumeBackup) *string
	GetVolBackupError(vb *harvesterv1.VolumeBackup) *harvesterv1.Error

	SetVolBackupReadyToUse(vb *harvesterv1.VolumeBackup, ready *bool) error
	SetVolBackupCreationTime(vb *harvesterv1.VolumeBackup, t *metav1.Time) error
	SetVolBackupError(vb *harvesterv1.VolumeBackup, err *snapshotv1.VolumeSnapshotError) error
	SetVolBackupProgress(vb *harvesterv1.VolumeBackup, p int) error
	SetVolBackupLHBackupName(vb *harvesterv1.VolumeBackup, name string) error

	Update(oldVMb, newVMb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error)
	UpdateByStatus(oldVMb, newVMb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error)

	IsReady(vmb *harvesterv1.VirtualMachineBackup) bool
	IsMissingStatus(vmb *harvesterv1.VirtualMachineBackup) bool
	IsProcessing(vmb *harvesterv1.VirtualMachineBackup) bool
	IsSyncErrMsg(vmb *harvesterv1.VirtualMachineBackup, errMsg string) bool
	IsTargetConsistent(vmb *harvesterv1.VirtualMachineBackup, target *settings.BackupTarget) bool
	IsTransitToNonReady(vmb *harvesterv1.VirtualMachineBackup) bool

	GetName(vmb *harvesterv1.VirtualMachineBackup) string
	GetNamespace(vmb *harvesterv1.VirtualMachineBackup) string
	GetKind(vmb *harvesterv1.VirtualMachineBackup) string
	GetUID(vmb *harvesterv1.VirtualMachineBackup) types.UID
	GetDeletionTimestap(vmb *harvesterv1.VirtualMachineBackup) *metav1.Time
	GetSpec(vmb *harvesterv1.VirtualMachineBackup) harvesterv1.VirtualMachineBackupSpec
	GetSourceSpec(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineSourceSpec
	GetStatus(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineBackupStatus
	GetSecretBackups(vmb *harvesterv1.VirtualMachineBackup) []harvesterv1.SecretBackup
	GetSourceVM(vmb *harvesterv1.VirtualMachineBackup) (*kubevirtv1.VirtualMachine, error)
	GetError(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.Error

	GetCSIDriverMap(vmb *harvesterv1.VirtualMachineBackup) (map[string]string, map[string]snapshotv1.VolumeSnapshotClass, error)
	GetType(vmb *harvesterv1.VirtualMachineBackup) harvesterv1.BackupType
	GetBackupTarget(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.BackupTarget

	SetCreationTime(vmb *harvesterv1.VirtualMachineBackup, t *metav1.Time) error
	SetError(vmb *harvesterv1.VirtualMachineBackup, err *harvesterv1.Error) error
	SetReadyToUse(vmb *harvesterv1.VirtualMachineBackup, ready bool) error
	SetProgress(vmb *harvesterv1.VirtualMachineBackup, p int) error

	ConfigureCSISnapClasses(old *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error)
	ConfigureBackupTargetOnStatus(old *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error)

	UpdateProcessingNoCommit(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineBackup
	UpdateChangeToNonReadyNoCommit(vmb *harvesterv1.VirtualMachineBackup, reason string) *harvesterv1.VirtualMachineBackup
	UpdateCompleteNoCommit(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineBackup
	UpdateErrorNoCommit(vmb *harvesterv1.VirtualMachineBackup, err error) *harvesterv1.VirtualMachineBackup

	UpdateCondition(old *harvesterv1.VirtualMachineBackup, c harvesterv1.Condition) (*harvesterv1.VirtualMachineBackup, error)
	UpdateError(old *harvesterv1.VirtualMachineBackup, err error) error
	UpdateMetadataReady(old *harvesterv1.VirtualMachineBackup) error

	InitVMBackup(old *harvesterv1.VirtualMachineBackup, vm *kubevirtv1.VirtualMachine) error

	TryFreezeFS(ctx context.Context, vmb *harvesterv1.VirtualMachineBackup) error
}

type vmbackupOperator struct {
	client      ctlharvesterv1.VirtualMachineBackupClient
	cache       ctlharvesterv1.VirtualMachineBackupCache
	vscCache    ctlsnapshotv1.VolumeSnapshotClassCache
	vmCache     ctlkubevirtv1.VirtualMachineCache
	vmiCache    ctlkubevirtv1.VirtualMachineInstanceCache
	pvcCache    ctlcorev1.PersistentVolumeClaimCache
	pvCache     ctlcorev1.PersistentVolumeCache
	secretCache ctlcorev1.SecretCache

	virtSubresourceRestClient rest.Interface
}

func GetVMBackupOperator(
	client ctlharvesterv1.VirtualMachineBackupClient,
	cache ctlharvesterv1.VirtualMachineBackupCache,
	vscCache ctlsnapshotv1.VolumeSnapshotClassCache,
	vmCache ctlkubevirtv1.VirtualMachineCache,
	vmiCache ctlkubevirtv1.VirtualMachineInstanceCache,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	pvCache ctlcorev1.PersistentVolumeCache,
	secretCache ctlcorev1.SecretCache,
	virtSubresourceRestClient rest.Interface,
) VMBackupOperator {
	return &vmbackupOperator{
		client,
		cache,
		vscCache,
		vmCache,
		vmiCache,
		pvcCache,
		pvCache,
		secretCache,
		virtSubresourceRestClient,
	}
}

func (vmbo *vmbackupOperator) GetVolBackups(vmb *harvesterv1.VirtualMachineBackup) []harvesterv1.VolumeBackup {
	if vmb.Status == nil {
		return nil
	}
	return vmb.Status.VolumeBackups
}

func (vmbo *vmbackupOperator) GetVolBackup(vmb *harvesterv1.VirtualMachineBackup, index int) *harvesterv1.VolumeBackup {
	if vmb.Status == nil || index < 0 || index >= len(vmb.Status.VolumeBackups) {
		return nil
	}
	return &vmb.Status.VolumeBackups[index]
}

func (vmbo *vmbackupOperator) GetSanitizeVolBackups(vmb *harvesterv1.VirtualMachineBackup) []harvesterv1.VolumeBackup {
	if vmb.Status == nil {
		return nil
	}
	sanitizeVolBackups := vmb.Status.VolumeBackups
	for i := range sanitizeVolBackups {
		sanitizeVolBackups[i].ReadyToUse = nil
		sanitizeVolBackups[i].CreationTime = nil
		sanitizeVolBackups[i].Error = nil
	}

	return sanitizeVolBackups
}

func (vmbo *vmbackupOperator) GetVolBackupName(vb *harvesterv1.VolumeBackup) *string {
	if vb == nil {
		return nil
	}
	return vb.Name
}

func (vmbo *vmbackupOperator) GetVolBackupPVCNameSpace(vb *harvesterv1.VolumeBackup) string {
	if vb == nil {
		return ""
	}
	return vb.PersistentVolumeClaim.ObjectMeta.Namespace
}

func (vmbo *vmbackupOperator) GetVolBackupPVCName(vb *harvesterv1.VolumeBackup) string {
	if vb == nil {
		return ""
	}
	return vb.PersistentVolumeClaim.ObjectMeta.Name
}

func (vmbo *vmbackupOperator) GetVolBackupVolumeName(vb *harvesterv1.VolumeBackup) string {
	if vb == nil {
		return ""
	}
	return vb.PersistentVolumeClaim.Spec.VolumeName
}

func (vmbo *vmbackupOperator) GetVolBackupCSIDriver(vb *harvesterv1.VolumeBackup) string {
	if vb == nil {
		return ""
	}
	return vb.CSIDriverName
}

func (vmbo *vmbackupOperator) GetVolBackupReadyToUse(vb *harvesterv1.VolumeBackup) bool {
	if vb == nil || vb.ReadyToUse == nil || !*vb.ReadyToUse {
		return false
	}
	return true
}

func (vmbo *vmbackupOperator) GetVolBackupSize(vb *harvesterv1.VolumeBackup) int64 {
	if vb == nil {
		return 0
	}
	return vb.VolumeSize
}

func (vmbo *vmbackupOperator) GetVolBackupProgress(vb *harvesterv1.VolumeBackup) int {
	if vb == nil {
		return 0
	}
	return vb.Progress
}

func (vmbo *vmbackupOperator) GetVolBackupLHBackupName(vb *harvesterv1.VolumeBackup) *string {
	if vb == nil {
		return nil
	}
	return vb.LonghornBackupName
}

func (vmbo *vmbackupOperator) GetVolBackupSCName(vb *harvesterv1.VolumeBackup) *string {
	if vb == nil {
		return nil
	}
	return vb.PersistentVolumeClaim.Spec.StorageClassName
}

func (vmbo *vmbackupOperator) GetVolBackupError(vb *harvesterv1.VolumeBackup) *harvesterv1.Error {
	if vb == nil {
		return nil
	}
	return vb.Error
}

func (vmbo *vmbackupOperator) SetVolBackupReadyToUse(vb *harvesterv1.VolumeBackup, ready *bool) error {
	if vb == nil {
		return fmt.Errorf("volume backup is nil")
	}
	vb.ReadyToUse = ready
	return nil
}

func (vmbo *vmbackupOperator) SetVolBackupCreationTime(vb *harvesterv1.VolumeBackup, t *metav1.Time) error {
	if vb == nil {
		return fmt.Errorf("volume backup is nil")
	}
	vb.CreationTime = t
	return nil
}

func translateError(e *snapshotv1.VolumeSnapshotError) *harvesterv1.Error {
	if e == nil {
		return nil
	}

	return &harvesterv1.Error{
		Message: e.Message,
		Time:    e.Time,
	}
}

func (vmbo *vmbackupOperator) SetVolBackupError(vb *harvesterv1.VolumeBackup, err *snapshotv1.VolumeSnapshotError) error {
	if vb == nil {
		return fmt.Errorf("volume backup is nil")
	}
	vb.Error = translateError(err)
	return nil
}

func (vmbo *vmbackupOperator) SetVolBackupProgress(vb *harvesterv1.VolumeBackup, p int) error {
	if vb == nil {
		return fmt.Errorf("volume backup is nil")
	}
	vb.Progress = p
	return nil
}

func (vmbo *vmbackupOperator) SetVolBackupLHBackupName(vb *harvesterv1.VolumeBackup, name string) error {
	if vb == nil {
		return fmt.Errorf("volume backup is nil")
	}
	vb.LonghornBackupName = ptr.To(name)
	return nil
}

func (vmbo *vmbackupOperator) Update(oldVMb, newVMb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error) {
	if reflect.DeepEqual(oldVMb, newVMb) {
		return newVMb, nil
	}

	return vmbo.client.Update(newVMb)
}

func (vmbo *vmbackupOperator) UpdateByStatus(oldVMb, newVMb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error) {
	if reflect.DeepEqual(oldVMb.Status, newVMb.Status) {
		return newVMb, nil
	}

	return vmbo.client.Update(newVMb)
}

func (vmbo *vmbackupOperator) IsReady(vmb *harvesterv1.VirtualMachineBackup) bool {
	return vmb.Status != nil && vmb.Status.ReadyToUse != nil && *vmb.Status.ReadyToUse
}

func (vmbo *vmbackupOperator) IsMissingStatus(vmb *harvesterv1.VirtualMachineBackup) bool {
	return vmb.Status == nil || vmb.Status.SourceSpec == nil || vmb.Status.VolumeBackups == nil
}

func (vmbo *vmbackupOperator) IsProcessing(vmb *harvesterv1.VirtualMachineBackup) bool {
	return vmbo.GetError(vmb) == nil &&
		(vmb.Status == nil || vmb.Status.ReadyToUse == nil || !*vmb.Status.ReadyToUse)
}

func (vmbo *vmbackupOperator) IsSyncErrMsg(vmb *harvesterv1.VirtualMachineBackup, errMsg string) bool {
	return vmb.Status != nil && vmb.Status.Error != nil && vmb.Status.Error.Message != nil && *vmb.Status.Error.Message == errMsg
}

func (vmbo *vmbackupOperator) IsTargetConsistent(vmb *harvesterv1.VirtualMachineBackup, target *settings.BackupTarget) bool {
	var bt *harvesterv1.BackupTarget
	if vmb.Status != nil {
		bt = vmb.Status.BackupTarget
	}

	if bt == nil && target == nil {
		return true
	}

	if bt == nil || target == nil {
		return false
	}

	return bt.Endpoint == target.Endpoint && bt.BucketName == target.BucketName && bt.BucketRegion == target.BucketRegion
}

func (vmbo *vmbackupOperator) IsTransitToNonReady(vmb *harvesterv1.VirtualMachineBackup) bool {
	if vmb.Status == nil {
		return false
	}
	for _, condition := range vmb.Status.Conditions {
		if condition.Type != harvesterv1.BackupConditionReady {
			continue
		}
		if condition.Message != changeToNonReadyMessage {
			continue
		}
		return true
	}
	return false
}

func (vmbo *vmbackupOperator) GetName(vmb *harvesterv1.VirtualMachineBackup) string {
	return vmb.Name
}

func (vmbo *vmbackupOperator) GetNamespace(vmb *harvesterv1.VirtualMachineBackup) string {
	return vmb.Namespace
}

func (vmbo *vmbackupOperator) GetKind(vmb *harvesterv1.VirtualMachineBackup) string {
	return vmb.Kind
}

func (vmbo *vmbackupOperator) GetUID(vmb *harvesterv1.VirtualMachineBackup) types.UID {
	return vmb.UID
}

func (vmbo *vmbackupOperator) GetDeletionTimestap(vmb *harvesterv1.VirtualMachineBackup) *metav1.Time {
	return vmb.DeletionTimestamp
}

func (vmbo *vmbackupOperator) GetSpec(vmb *harvesterv1.VirtualMachineBackup) harvesterv1.VirtualMachineBackupSpec {
	return vmb.Spec
}

func (vmbo *vmbackupOperator) GetStatus(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineBackupStatus {
	return vmb.Status
}

func (vmbo *vmbackupOperator) GetSourceSpec(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineSourceSpec {
	if vmb.Status == nil {
		return nil
	}
	return vmb.Status.SourceSpec
}

func (vmbo *vmbackupOperator) GetSecretBackups(vmb *harvesterv1.VirtualMachineBackup) []harvesterv1.SecretBackup {
	if vmb.Status == nil {
		return nil
	}
	return vmb.Status.SecretBackups
}

func (vmbo *vmbackupOperator) GetSourceVM(vmb *harvesterv1.VirtualMachineBackup) (*kubevirtv1.VirtualMachine, error) {
	var (
		sourceVM *kubevirtv1.VirtualMachine
		err      error
	)

	switch vmb.Spec.Source.Kind {
	case kubevirtv1.VirtualMachineGroupVersionKind.Kind:
		sourceVM, err = vmbo.vmCache.Get(vmb.Namespace, vmb.Spec.Source.Name)
	default:
		err = fmt.Errorf("unsupported source: %+v", vmb.Spec.Source)
	}

	if err != nil {
		return nil, err
	}

	if sourceVM.DeletionTimestamp != nil {
		return nil, fmt.Errorf("vm %s/%s is being deleted", vmb.Namespace, vmb.Spec.Source.Name)
	}
	return sourceVM, nil
}

func (vmbo *vmbackupOperator) GetError(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.Error {
	if vmb.Status != nil && vmb.Status.Error != nil {
		return vmb.Status.Error
	}
	return nil
}

func (vmbo *vmbackupOperator) GetCSIDriverMap(vmb *harvesterv1.VirtualMachineBackup) (map[string]string, map[string]snapshotv1.VolumeSnapshotClass, error) {
	// Early return if Status is nil or no volume backups to process
	if vmb.Status == nil || len(vmb.Status.VolumeBackups) == 0 {
		return map[string]string{}, map[string]snapshotv1.VolumeSnapshotClass{}, nil
	}

	csiDriverConfig, err := vmbo.loadCSIDriverConfig()
	if err != nil {
		return nil, nil, err
	}

	csiDriverSnapClassNameMap := map[string]string{}
	csiDriverSnapClassMap := map[string]snapshotv1.VolumeSnapshotClass{}

	for _, vb := range vmb.Status.VolumeBackups {
		csiDriverName := vb.CSIDriverName

		// Skip if already processed this CSI driver
		if _, processed := csiDriverSnapClassNameMap[csiDriverName]; processed {
			continue
		}

		// Process the CSI driver and get its snapshot class
		volumeSnapClassName, volumeSnapshotClass, err := vmbo.processCSIDriver(
			csiDriverName,
			vmb.Spec.Type,
			csiDriverConfig,
		)
		if err != nil {
			return nil, nil, err
		}

		csiDriverSnapClassNameMap[csiDriverName] = volumeSnapClassName
		csiDriverSnapClassMap[csiDriverName] = *volumeSnapshotClass
	}

	return csiDriverSnapClassNameMap, csiDriverSnapClassMap, nil
}

func (vmbo *vmbackupOperator) GetType(vmb *harvesterv1.VirtualMachineBackup) harvesterv1.BackupType {
	return vmb.Spec.Type
}

func (vmbo *vmbackupOperator) GetBackupTarget(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.BackupTarget {
	if vmb.Status == nil {
		return nil
	}
	return vmb.Status.BackupTarget
}

func (vmbo *vmbackupOperator) SetCreationTime(vmb *harvesterv1.VirtualMachineBackup, t *metav1.Time) error {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}
	vmb.Status.CreationTime = t
	return nil
}

func (vmbo *vmbackupOperator) SetError(vmb *harvesterv1.VirtualMachineBackup, err *harvesterv1.Error) error {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}
	vmb.Status.Error = err
	return nil
}

func (vmbo *vmbackupOperator) SetReadyToUse(vmb *harvesterv1.VirtualMachineBackup, ready bool) error {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}
	vmb.Status.ReadyToUse = ptr.To(ready)
	return nil
}

func (vmbo *vmbackupOperator) SetProgress(vmb *harvesterv1.VirtualMachineBackup, p int) error {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}
	vmb.Status.Progress = p
	return nil
}

// loadCSIDriverConfig loads and merges default and custom CSI driver configurations.
// Custom configurations take precedence over defaults.
func (vmbo *vmbackupOperator) loadCSIDriverConfig() (map[string]settings.CSIDriverInfo, error) {
	// Load default CSI driver configuration
	defaultConfig := map[string]settings.CSIDriverInfo{}
	if err := json.Unmarshal([]byte(settings.CSIDriverConfig.GetDefault()), &defaultConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal default CSI driver config: %w, value: %s", err, settings.CSIDriverConfig.GetDefault())
	}

	// Load custom CSI driver configuration
	customConfig := map[string]settings.CSIDriverInfo{}
	if err := json.Unmarshal([]byte(settings.CSIDriverConfig.Get()), &customConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal custom CSI driver config: %w, value: %s", err, settings.CSIDriverConfig.Get())
	}

	// Merge configurations: add custom drivers not present in defaults
	for driverName, driverInfo := range customConfig {
		if _, exists := defaultConfig[driverName]; !exists {
			defaultConfig[driverName] = driverInfo
		}
	}

	return defaultConfig, nil
}

// getVolumeSnapshotClassName determines the appropriate snapshot class name based on backup type
func getVSCName(backupType harvesterv1.BackupType, driverInfo settings.CSIDriverInfo, csiDriverName string) (string, error) {
	var vscName string

	switch backupType {
	case harvesterv1.Backup:
		vscName = driverInfo.BackupVolumeSnapshotClassName
	case harvesterv1.Snapshot:
		vscName = driverInfo.VolumeSnapshotClassName
	default:
		return "", fmt.Errorf("unsupported backup type %q for CSI driver %q", backupType, csiDriverName)
	}

	if vscName == "" {
		return "", fmt.Errorf("no volume snapshot class configured for CSI driver %q with backup type %q",
			csiDriverName, backupType)
	}

	return vscName, nil
}

// processCSIDriver processes a single CSI driver and retrieves its volume snapshot class
func (vmbo *vmbackupOperator) processCSIDriver(
	csiDriverName string,
	backupType harvesterv1.BackupType,
	csiDriverConfig map[string]settings.CSIDriverInfo,
) (string, *snapshotv1.VolumeSnapshotClass, error) {
	// Validate CSI driver exists in configuration
	driverInfo, ok := csiDriverConfig[csiDriverName]
	if !ok {
		return "", nil, fmt.Errorf("CSI driver %q not found in CSIDriverInfo settings. Available drivers: %v",
			csiDriverName, getAvailableDriverNames(csiDriverConfig))
	}

	// Determine the appropriate volume snapshot class name
	vscName, err := getVSCName(backupType, driverInfo, csiDriverName)
	if err != nil {
		return "", nil, err
	}

	// Retrieve the volume snapshot class
	vsc, err := vmbo.vscCache.Get(vscName)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get VolumeSnapshotClass %q for CSI driver %q: %w",
			vscName, csiDriverName, err)
	}

	return vscName, vsc, nil
}

// getAvailableDriverNames returns a list of available CSI driver names for error messages
func getAvailableDriverNames(config map[string]settings.CSIDriverInfo) []string {
	names := make([]string, 0, len(config))
	for name := range config {
		names = append(names, name)
	}
	return names
}

func (vmbo *vmbackupOperator) ConfigureCSISnapClasses(old *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error) {
	if old.Status != nil && len(old.Status.CSIDriverVolumeSnapshotClassNames) != 0 {
		return old, nil
	}

	newVMb := old.DeepCopy()
	csiDriverSnapClassNameMap, _, err := vmbo.GetCSIDriverMap(newVMb)
	if err != nil {
		return nil, err
	}

	if newVMb.Status == nil {
		newVMb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}
	newVMb.Status.CSIDriverVolumeSnapshotClassNames = csiDriverSnapClassNameMap
	return vmbo.Update(old, newVMb)
}

func (vmbo *vmbackupOperator) isBackupTargetOnAnnotation(vmb *harvesterv1.VirtualMachineBackup) bool {
	return vmb.Annotations != nil &&
		(vmb.Annotations[backupTargetAnnotation] != "" ||
			vmb.Annotations[backupBucketNameAnnotation] != "" ||
			vmb.Annotations[backupBucketRegionAnnotation] != "")
}

func (vmbo *vmbackupOperator) ConfigureBackupTargetOnStatus(old *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error) {
	if !vmbo.isBackupTargetOnAnnotation(old) {
		return old, nil
	}

	newVMb := old.DeepCopy()
	newVMb.Status.BackupTarget = &harvesterv1.BackupTarget{
		Endpoint:     newVMb.Annotations[backupTargetAnnotation],
		BucketName:   newVMb.Annotations[backupBucketNameAnnotation],
		BucketRegion: newVMb.Annotations[backupBucketRegionAnnotation],
	}
	return vmbo.Update(old, newVMb)
}

var currentTime = func() *metav1.Time {
	t := metav1.Now()
	return &t
}

func newReadyCondition(status corev1.ConditionStatus, reason string, message string) harvesterv1.Condition {
	return harvesterv1.Condition{
		Type:               harvesterv1.BackupConditionReady,
		Status:             status,
		Message:            message,
		Reason:             reason,
		LastTransitionTime: currentTime().Format(time.RFC3339),
	}
}

func newProgressingCondition(status corev1.ConditionStatus, reason string, message string) harvesterv1.Condition {
	return harvesterv1.Condition{
		Type:   harvesterv1.BackupConditionProgressing,
		Status: status,
		// wrangler use Reason to determine whether an object is in error state.
		// ref: https://github.com/rancher/wrangler/blob/6970ad98ba7bd2755312ccfc6540a92bc9a9e316/pkg/summary/summarizers.go#L220-L243
		Reason:             reason,
		Message:            message,
		LastTransitionTime: currentTime().Format(time.RFC3339),
	}
}

func (vmbo *vmbackupOperator) applyCondition(vmb *harvesterv1.VirtualMachineBackup, c harvesterv1.Condition) *harvesterv1.VirtualMachineBackup {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}

	conditions := vmb.Status.Conditions

	found := false
	for i := range conditions {
		if conditions[i].Type == c.Type {
			if conditions[i].Status != c.Status || (conditions[i].Reason != c.Reason) || (conditions[i].Message != c.Message) {
				conditions[i] = c
			}
			found = true
			break
		}
	}

	if !found {
		conditions = append(conditions, c)
	}

	vmb.Status.Conditions = conditions
	return vmb
}

func (vmbo *vmbackupOperator) UpdateProcessingNoCommit(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineBackup {
	vmb = vmbo.applyCondition(vmb, newProgressingCondition(corev1.ConditionTrue, "", "Operation in progress"))
	vmb = vmbo.applyCondition(vmb, newReadyCondition(corev1.ConditionFalse, "", "Not ready"))
	return vmb
}

func (vmbo *vmbackupOperator) UpdateChangeToNonReadyNoCommit(
	vmb *harvesterv1.VirtualMachineBackup,
	reason string,
) *harvesterv1.VirtualMachineBackup {
	return vmbo.applyCondition(vmb, newReadyCondition(corev1.ConditionFalse, reason, changeToNonReadyMessage))
}

func (vmbo *vmbackupOperator) UpdateCompleteNoCommit(vmb *harvesterv1.VirtualMachineBackup) *harvesterv1.VirtualMachineBackup {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}
	vmb.Status.CreationTime = currentTime()
	vmb.Status.Error = nil
	vmb = vmbo.applyCondition(vmb, newProgressingCondition(corev1.ConditionFalse, "", "Operation complete"))
	vmb = vmbo.applyCondition(vmb, newReadyCondition(corev1.ConditionTrue, "", "Operation complete"))
	return vmb
}

func (vmbo *vmbackupOperator) UpdateErrorNoCommit(vmb *harvesterv1.VirtualMachineBackup, err error) *harvesterv1.VirtualMachineBackup {
	if vmb.Status == nil {
		vmb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}

	vmb.Status.Error = &harvesterv1.Error{
		Time:    currentTime(),
		Message: ptr.To(err.Error()),
	}

	vmb = vmbo.applyCondition(vmb, newProgressingCondition(corev1.ConditionFalse, "Error", err.Error()))
	vmb = vmbo.applyCondition(vmb, newReadyCondition(corev1.ConditionFalse, "", "Not Ready"))
	return vmb
}

func (vmbo *vmbackupOperator) UpdateCondition(old *harvesterv1.VirtualMachineBackup, c harvesterv1.Condition) (*harvesterv1.VirtualMachineBackup, error) {
	newVMb := old.DeepCopy()
	newVMb = vmbo.applyCondition(newVMb, c)
	return vmbo.Update(old, newVMb)
}

func (vmbo *vmbackupOperator) UpdateError(old *harvesterv1.VirtualMachineBackup, err error) error {
	newVMb := old.DeepCopy()
	newVMb = vmbo.UpdateErrorNoCommit(newVMb, err)

	_, updateErr := vmbo.Update(old, newVMb)
	return updateErr
}

func (vmbo *vmbackupOperator) UpdateMetadataReady(old *harvesterv1.VirtualMachineBackup) error {
	newVMb := old.DeepCopy()
	if newVMb.Status == nil {
		newVMb.Status = &harvesterv1.VirtualMachineBackupStatus{}
	}

	newVMb = vmbo.applyCondition(newVMb, harvesterv1.Condition{
		Type:               harvesterv1.BackupConditionMetadataReady,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: currentTime().Format(time.RFC3339),
	})

	_, err := vmbo.Update(old, newVMb)
	return err
}

func (vmbo *vmbackupOperator) initStatus(vmb *harvesterv1.VirtualMachineBackup, vm *kubevirtv1.VirtualMachine) *harvesterv1.VirtualMachineBackup {
	vmb.Status = &harvesterv1.VirtualMachineBackupStatus{
		ReadyToUse: pointer.BoolPtr(false),
		SourceUID:  &vm.UID,
		SourceSpec: &harvesterv1.VirtualMachineSourceSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name:        vm.ObjectMeta.Name,
				Namespace:   vm.ObjectMeta.Namespace,
				Annotations: vm.ObjectMeta.Annotations,
				Labels:      vm.ObjectMeta.Labels,
			},
			Spec: vm.Spec,
		},
	}
	return vmb
}

// volumeToPVCMappings extracts PVC mappings from VM volumes
// Returns a map of volume name to PVC name for volumes backed by PVCs
func volumeToPVCMappings(volumes []kubevirtv1.Volume) map[string]string {
	if len(volumes) == 0 {
		return map[string]string{}
	}

	pvcs := make(map[string]string, len(volumes))

	for _, volume := range volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			pvcs[volume.Name] = volume.PersistentVolumeClaim.ClaimName
		}
	}

	return pvcs
}

func (vmbo *vmbackupOperator) getPVC(namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	pvc, err := vmbo.pvcCache.Get(namespace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get PVC %s/%s: %w", namespace, name, err)
	}

	if pvc.Spec.VolumeName == "" {
		return nil, fmt.Errorf("PVC %s/%s is not bound to a PV", pvc.Namespace, pvc.Name)
	}

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil, fmt.Errorf("PVC %s/%s has no storage class configured", pvc.Namespace, pvc.Name)
	}

	return pvc, nil
}

// validatePVForBackup checks if a PV is suitable for backup operations
func validatePVForBackup(pv *corev1.PersistentVolume, backupType harvesterv1.BackupType) error {
	if pv.Spec.PersistentVolumeSource.CSI == nil {
		return fmt.Errorf("PV %s is not CSI-backed, cannot create %s", pv.Name, backupType)
	}
	return nil
}

// getVolumeSize extracts the storage capacity from a PV
func getVolumeSize(pv *corev1.PersistentVolume) (int64, error) {
	storageCapacity, exists := pv.Spec.Capacity[corev1.ResourceStorage]
	if !exists {
		return 0, fmt.Errorf("PV %s has no storage capacity defined", pv.Name)
	}
	return storageCapacity.Value(), nil
}

// buildVolumeBackup constructs a VolumeBackup object from PVC and PV information
func buildVolumeBackup(backupName, volName, pvcName string, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, volSize int64) *harvesterv1.VolumeBackup {
	volumeBackupName := fmt.Sprintf(volumeBackupNameFormat, backupName, pvcName)

	return &harvesterv1.VolumeBackup{
		Name:          &volumeBackupName,
		VolumeName:    volName,
		CSIDriverName: pv.Spec.PersistentVolumeSource.CSI.Driver,
		PersistentVolumeClaim: harvesterv1.PersistentVolumeClaimSourceSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pvc.ObjectMeta.Name,
				Namespace:   pvc.ObjectMeta.Namespace,
				Labels:      pvc.Labels,
				Annotations: pvc.Annotations,
			},
			Spec: pvc.Spec,
		},
		ReadyToUse: ptr.To(false),
		VolumeSize: volSize,
	}
}

func (vmbo *vmbackupOperator) createVolumeBackup(backup *harvesterv1.VirtualMachineBackup, volName, pvcName, namespace string) (*harvesterv1.VolumeBackup, error) {
	pvc, err := vmbo.getPVC(namespace, pvcName)
	if err != nil {
		return nil, fmt.Errorf("volume %s: %w", volName, err)
	}

	pv, err := vmbo.pvCache.Get(pvc.Spec.VolumeName)
	if err != nil {
		return nil, fmt.Errorf("volume %s: failed to get PV %s: %w", volName, pvc.Spec.VolumeName, err)
	}

	if err := validatePVForBackup(pv, backup.Spec.Type); err != nil {
		return nil, fmt.Errorf("volume %s: %w", volName, err)
	}

	volSize, err := getVolumeSize(pv)
	if err != nil {
		return nil, fmt.Errorf("volume %s: %w", volName, err)
	}

	vb := buildVolumeBackup(backup.Name, volName, pvcName, pvc, pv, volSize)
	return vb, nil
}

func (vmbo *vmbackupOperator) initVolBackups(vmb *harvesterv1.VirtualMachineBackup, vm *kubevirtv1.VirtualMachine) (*harvesterv1.VirtualMachineBackup, error) {
	srcVols := vm.Spec.Template.Spec.Volumes
	pvcMappings := volumeToPVCMappings(srcVols)

	if len(pvcMappings) == 0 {
		return vmb, nil
	}

	volBackups := make([]harvesterv1.VolumeBackup, 0, len(pvcMappings))

	for volName, pvcName := range pvcMappings {
		vb, err := vmbo.createVolumeBackup(vmb, volName, pvcName, vm.Namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to create volume backup for VM %s/%s: %w", vm.Namespace, vm.Name, err)
		}

		volBackups = append(volBackups, *vb)
	}

	vmb.Status.VolumeBackups = volBackups
	return vmb, nil
}

// getSecretBackupFromSecret retrieves a secret and converts it to a SecretBackup.
// It removes empty data values to avoid errors during backup operations.
// ref: https://github.com/harvester/harvester/issues/1536
func (vmbo *vmbackupOperator) getSecretBackupFromSecret(namespace, name string) (*harvesterv1.SecretBackup, error) {
	secret, err := vmbo.secretCache.Get(namespace, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, name, err)
	}

	// Remove empty values to avoid backup errors
	data := make(map[string][]byte, len(secret.Data))
	for k, v := range secret.Data {
		if len(v) > 0 {
			data[k] = v
		}
	}

	return &harvesterv1.SecretBackup{Name: secret.Name, Data: data}, nil
}

// collectSecretRefsFromAccessCredentials extracts secret references from VM access credentials
func collectSecretRefsFromAccessCredentials(credentials []kubevirtv1.AccessCredential) []string {
	var secretNames []string

	for _, credential := range credentials {
		if credential.SSHPublicKey != nil &&
			credential.SSHPublicKey.Source.Secret != nil {
			secretNames = append(secretNames, credential.SSHPublicKey.Source.Secret.SecretName)
		}
		if credential.UserPassword != nil &&
			credential.UserPassword.Source.Secret != nil {
			secretNames = append(secretNames, credential.UserPassword.Source.Secret.SecretName)
		}
	}

	return secretNames
}

// collectSecretRefsFromVolumes extracts secret references from VM volumes
func collectSecretRefsFromVolumes(volumes []kubevirtv1.Volume) []string {
	var secretNames []string

	for _, volume := range volumes {
		if volume.CloudInitNoCloud != nil {
			if volume.CloudInitNoCloud.UserDataSecretRef != nil {
				secretNames = append(secretNames, volume.CloudInitNoCloud.UserDataSecretRef.Name)
			}
			if volume.CloudInitNoCloud.NetworkDataSecretRef != nil {
				secretNames = append(secretNames, volume.CloudInitNoCloud.NetworkDataSecretRef.Name)
			}
		}
	}

	return secretNames
}

// getSecretBackups collects all secrets referenced by the VM and creates SecretBackup objects.
// It deduplicates secret references to avoid backing up the same secret multiple times.
// Returns an error if any required secret cannot be retrieved.
func (vmbo *vmbackupOperator) initSecretBackups(vmb *harvesterv1.VirtualMachineBackup, vm *kubevirtv1.VirtualMachine) (*harvesterv1.VirtualMachineBackup, error) {
	// Collect all secret references from different sources
	secretNames := []string{}
	secretNames = append(secretNames, collectSecretRefsFromAccessCredentials(vm.Spec.Template.Spec.AccessCredentials)...)
	secretNames = append(secretNames, collectSecretRefsFromVolumes(vm.Spec.Template.Spec.Volumes)...)

	if len(secretNames) == 0 {
		vmb.Status.SecretBackups = []harvesterv1.SecretBackup{}
		return vmb, nil
	}

	// Deduplicate secret names using a set
	seenSecrets := make(map[string]struct{}, len(secretNames))
	secretBackups := make([]harvesterv1.SecretBackup, 0, len(secretNames))

	for _, secretName := range secretNames {
		secretFullName := fmt.Sprintf("%s/%s", vm.Namespace, secretName)

		// Skip if already processed
		if _, seen := seenSecrets[secretFullName]; seen {
			continue
		}

		secretBackup, err := vmbo.getSecretBackupFromSecret(vm.Namespace, secretName)
		if err != nil {
			return nil, fmt.Errorf("failed to backup secret for VM %s/%s: %w", vm.Namespace, vm.Name, err)
		}

		seenSecrets[secretFullName] = struct{}{}
		secretBackups = append(secretBackups, *secretBackup)
	}

	vmb.Status.SecretBackups = secretBackups
	return vmb, nil
}

func (vmbo *vmbackupOperator) initVSCNames(vmb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error) {
	vscNames, _, err := vmbo.GetCSIDriverMap(vmb)
	if err != nil {
		return nil, err
	}

	vmb.Status.CSIDriverVolumeSnapshotClassNames = vscNames
	return vmb, nil
}

func (vmbo *vmbackupOperator) initBackupTarget(vmb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, error) {
	target, err := settings.DecodeBackupTarget(settings.BackupTargetSet.Get())
	if err != nil {
		return nil, err
	}

	vmb.Status.BackupTarget = &harvesterv1.BackupTarget{
		Endpoint:     target.Endpoint,
		BucketName:   target.BucketName,
		BucketRegion: target.BucketRegion,
	}
	return vmb, nil
}

func (vmbo *vmbackupOperator) InitVMBackup(old *harvesterv1.VirtualMachineBackup, vm *kubevirtv1.VirtualMachine) error {
	newVMb := old.DeepCopy()
	newVMb = vmbo.initStatus(newVMb, vm)

	newVMb, err := vmbo.initVolBackups(newVMb, vm)
	if err != nil {
		return err
	}

	newVMb, err = vmbo.initSecretBackups(newVMb, vm)
	if err != nil {
		return err
	}

	newVMb, err = vmbo.initVSCNames(newVMb)
	if err != nil {
		return err
	}

	if vmbo.GetType(newVMb) == harvesterv1.Backup {
		newVMb, err = vmbo.initBackupTarget(newVMb)
		if err != nil {
			return err
		}
	}

	_, err = vmbo.Update(old, newVMb)
	return err
}

func (vmbo *vmbackupOperator) getBackupSourceInstance(vmb *harvesterv1.VirtualMachineBackup) (*kubevirtv1.VirtualMachineInstance, error) {
	if vmb.Spec.Source.Kind != kubevirtv1.VirtualMachineGroupVersionKind.Kind {
		return nil, fmt.Errorf("unsupported source: %+v", vmb.Spec.Source)
	}

	sourceVMI, err := vmbo.vmiCache.Get(vmb.Namespace, vmb.Spec.Source.Name)
	if err != nil {
		return nil, err
	}
	if sourceVMI.DeletionTimestamp != nil {
		return nil, fmt.Errorf("vm %s/%s is being deleted", vmb.Namespace, vmb.Spec.Source.Name)
	}

	return sourceVMI, nil
}

func (vmbo *vmbackupOperator) saveFreezeFSAnnotation(old *harvesterv1.VirtualMachineBackup, value bool) (*harvesterv1.VirtualMachineBackup, error) {
	newVMb := old.DeepCopy()
	if newVMb.Annotations == nil {
		newVMb.Annotations = make(map[string]string)
	}

	newVMb.Annotations[util.AnnotationSnapshotFreezeFS] = strconv.FormatBool(value)
	return vmbo.Update(old, newVMb)
}

func (vmbo *vmbackupOperator) withFreezeFSAnnotation(vmb *harvesterv1.VirtualMachineBackup) (*harvesterv1.VirtualMachineBackup, *kubevirtv1.VirtualMachineInstance, error) {
	sourceVMI, err := vmbo.getBackupSourceInstance(vmb)
	if apierrors.IsNotFound(err) {
		updatedVMb, updateErr := vmbo.saveFreezeFSAnnotation(vmb, false)
		return updatedVMb, nil, updateErr
	}
	if err != nil {
		return vmb, nil, err
	}

	hasAgent := false

	for _, condition := range sourceVMI.Status.Conditions {
		if condition.Type == kubevirtv1.VirtualMachineInstanceAgentConnected {
			hasAgent = condition.Status == corev1.ConditionTrue
			break
		}
	}

	updatedVMb, err := vmbo.saveFreezeFSAnnotation(vmb, hasAgent)
	return updatedVMb, sourceVMI, err
}

func (vmbo *vmbackupOperator) freezeFS(ctx context.Context, vmi *kubevirtv1.VirtualMachineInstance, timeout time.Duration) error {
	body, err := json.Marshal(kubevirtv1.FreezeUnfreezeTimeout{UnfreezeTimeout: &metav1.Duration{
		Duration: timeout,
	}})
	if err != nil {
		return err
	}

	res := vmbo.virtSubresourceRestClient.Put().
		Namespace(vmi.Namespace).
		Resource("virtualmachineinstances").
		Name(vmi.Name).
		SubResource("freeze").
		Body(body).
		Do(ctx)
	return res.Error()
}

func (vmbo *vmbackupOperator) TryFreezeFS(ctx context.Context, vmb *harvesterv1.VirtualMachineBackup) error {
	updatedVMb, sourceVMI, err := vmbo.withFreezeFSAnnotation(vmb)
	if err != nil {
		return fmt.Errorf("failed to configure freeze annotation: %w", err)
	}

	freezeText := updatedVMb.Annotations[util.AnnotationSnapshotFreezeFS]
	freeze, err := strconv.ParseBool(freezeText)
	if err != nil {
		return fmt.Errorf("invalid freeze annotation %q=%q: %w", util.AnnotationSnapshotFreezeFS, freezeText, err)
	}

	if !freeze {
		logrus.Debugf("Skipping filesystem freeze for VM %s/%s: qemu-guest-agent not available", vmb.Namespace, vmb.Spec.Source.Name)
		return nil
	}

	// sourceVMI is nil if VM was not found (already handled by annotation being set to false)
	if sourceVMI == nil {
		return nil
	}

	if err := vmbo.freezeFS(ctx, sourceVMI, defaultFreezeDuration); err != nil {
		return fmt.Errorf("qemu-guest-agent failed to freeze filesystem for VM %s/%s: %w",
			sourceVMI.Namespace, sourceVMI.Name, err)
	}

	logrus.Infof("Successfully froze filesystem for VM %s/%s", sourceVMI.Namespace, sourceVMI.Name)
	return nil
}
