package backup

// Harvester VM backup & restore controllers helps to manage the VM backup & restore by leveraging
// the VolumeSnapshot functionality of Kubernetes CSI drivers with built-in storage driver longhorn.
// Currently, the following features are supported:
// 1. support VM live & offline backup to the supported backupTarget(i.e, nfs_v4 or s3 storage server).
// 2. restore a backup to a new VM or replacing it with the existing VM is supported.
import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	lhdatastore "github.com/longhorn/longhorn-manager/datastore"
	lhv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	"github.com/longhorn/longhorn-manager/types"
	lhutil "github.com/longhorn/longhorn-manager/util"
	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/storage/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	kubevirtv1 "kubevirt.io/api/core/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	backupcommon "github.com/harvester/harvester/pkg/backup/common"
	"github.com/harvester/harvester/pkg/config"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
	ctlkubevirtv1 "github.com/harvester/harvester/pkg/generated/controllers/kubevirt.io/v1"
	ctllhv1 "github.com/harvester/harvester/pkg/generated/controllers/longhorn.io/v1beta2"
	ctlsnapshotv1 "github.com/harvester/harvester/pkg/generated/controllers/snapshot.storage.k8s.io/v1"
	restorecommon "github.com/harvester/harvester/pkg/restore/common"
	"github.com/harvester/harvester/pkg/restore/engine"
	"github.com/harvester/harvester/pkg/restore/longhorn"
	restoresnapshot "github.com/harvester/harvester/pkg/restore/snapshot"
	"github.com/harvester/harvester/pkg/util"
)

const (
	restoreControllerName = "harvester-vm-restore-controller"

	restoreNameAnnotation  = "restore.harvesterhci.io/name"
	lastRestoreAnnotation  = "restore.harvesterhci.io/last-restore-uid"
	pvcNameSpaceAnnotation = "pvc.harvesterhci.io/namespace"
	pvcNameAnnotation      = "pvc.harvesterhci.io/name"

	pvNamePrefix = "pvc"

	//not truncate or remove dashes pv name
	volumeNameUUIDNoTruncate = -1
)

type RestoreHandler struct {
	context context.Context

	// Controllers and clients still directly used
	vmrController ctlharvesterv1.VirtualMachineRestoreController
	vmrCache      ctlharvesterv1.VirtualMachineRestoreCache
	vmClient      ctlkubevirtv1.VirtualMachineClient
	vscClient     ctlsnapshotv1.VolumeSnapshotContentClient
	vscCache      ctlsnapshotv1.VolumeSnapshotContentCache
	volumeCache   ctllhv1.VolumeCache
	volumes       ctllhv1.VolumeClient
	scCache       ctlstoragev1.StorageClassCache

	// Operators and engines
	vmbo    backupcommon.VMBackupOperator
	vmro    restorecommon.VMRestoreOperator
	engines map[harvesterv1.BackupType]engine.RestoreEngine
}

func RegisterRestore(ctx context.Context, management *config.Management, _ config.Options) error {
	// Get all required controllers and caches
	controllers := getRestoreControllers(management)

	// Initialize REST client for Kubevirt
	restClient, err := newKubevirtClient(management.RestConfig)
	if err != nil {
		return err
	}

	// Initialize operators
	vmbo, vmro := newRestoreOperators(controllers, restClient)

	// Initialize restore engines
	engines := newRestoreEngines(controllers, vmbo, vmro)

	// Create and configure handler
	handler := newRestoreHandler(ctx, controllers, vmbo, vmro, engines)

	// Register event handlers
	registerRestoreEventHandlers(ctx, controllers, handler)

	return nil
}

// restoreControllerSet holds all required controllers and caches
type restoreControllerSet struct {
	vmrs            ctlharvesterv1.VirtualMachineRestoreController
	vmbs            ctlharvesterv1.VirtualMachineBackupController
	vms             ctlkubevirtv1.VirtualMachineController
	vmis            ctlkubevirtv1.VirtualMachineInstanceController
	pvcs            ctlcorev1.PersistentVolumeClaimController
	pvs             ctlcorev1.PersistentVolumeController
	scs             ctlstoragev1.StorageClassController
	secrets         ctlcorev1.SecretController
	vss             ctlsnapshotv1.VolumeSnapshotController
	vscs            ctlsnapshotv1.VolumeSnapshotContentController
	lhbackups       ctllhv1.BackupController
	lhbackupVolumes ctllhv1.BackupVolumeController
	volumes         ctllhv1.VolumeController
	lhengines       ctllhv1.EngineController
	vsClasses       ctlsnapshotv1.VolumeSnapshotClassController
}

// getRestoreControllers extracts all required controllers from management
func getRestoreControllers(management *config.Management) *restoreControllerSet {
	return &restoreControllerSet{
		vmrs:            management.HarvesterFactory.Harvesterhci().V1beta1().VirtualMachineRestore(),
		vmbs:            management.HarvesterFactory.Harvesterhci().V1beta1().VirtualMachineBackup(),
		vms:             management.VirtFactory.Kubevirt().V1().VirtualMachine(),
		vmis:            management.VirtFactory.Kubevirt().V1().VirtualMachineInstance(),
		pvcs:            management.CoreFactory.Core().V1().PersistentVolumeClaim(),
		pvs:             management.CoreFactory.Core().V1().PersistentVolume(),
		scs:             management.StorageFactory.Storage().V1().StorageClass(),
		secrets:         management.CoreFactory.Core().V1().Secret(),
		vss:             management.SnapshotFactory.Snapshot().V1().VolumeSnapshot(),
		vscs:            management.SnapshotFactory.Snapshot().V1().VolumeSnapshotContent(),
		lhbackups:       management.LonghornFactory.Longhorn().V1beta2().Backup(),
		lhbackupVolumes: management.LonghornFactory.Longhorn().V1beta2().BackupVolume(),
		volumes:         management.LonghornFactory.Longhorn().V1beta2().Volume(),
		lhengines:       management.LonghornFactory.Longhorn().V1beta2().Engine(),
		vsClasses:       management.SnapshotFactory.Snapshot().V1().VolumeSnapshotClass(),
	}
}

// newRestoreOperators creates and configures VMBackup and VMRestore operators
func newRestoreOperators(
	controllers *restoreControllerSet,
	restClient *rest.RESTClient,
) (backupcommon.VMBackupOperator, restorecommon.VMRestoreOperator) {
	vmbo := backupcommon.GetVMBackupOperator(
		nil, // client not needed for restore operations
		controllers.vmbs.Cache(),
		controllers.vsClasses.Cache(),
		controllers.vms.Cache(),
		controllers.vmis.Cache(),
		controllers.pvcs.Cache(),
		controllers.pvs.Cache(),
		controllers.secrets.Cache(),
		nil, // virtSubresourceRestClient not needed
	)

	vmro := restorecommon.GetVMRestoreOperator(
		controllers.vmrs,
		controllers.vmrs.Cache(),
		controllers.vms.Cache(),
		controllers.vmis.Cache(),
		controllers.pvcs,
		controllers.pvcs.Cache(),
		controllers.secrets,
		controllers.secrets.Cache(),
		controllers.vmbs.Cache(),
		vmbo,
		restClient,
	)

	return vmbo, vmro
}

// newRestoreEngines creates restore engines for different backup types
func newRestoreEngines(
	controllers *restoreControllerSet,
	vmbo backupcommon.VMBackupOperator,
	vmro restorecommon.VMRestoreOperator,
) map[harvesterv1.BackupType]engine.RestoreEngine {
	return map[harvesterv1.BackupType]engine.RestoreEngine{
		harvesterv1.Backup: longhorn.GetRestoreEngine(
			vmbo,
			vmro,
			controllers.pvcs.Cache(),
			controllers.pvcs,
			controllers.scs.Cache(),
			controllers.vss.Cache(),
			controllers.vss,
			controllers.vscs.Cache(),
			controllers.vscs,
			controllers.lhbackups.Cache(),
			controllers.lhbackupVolumes.Cache(),
			controllers.lhengines.Cache(),
			controllers.volumes.Cache(),
			controllers.vmbs,
			controllers.vmbs.Cache(),
		),
		harvesterv1.Snapshot: restoresnapshot.GetRestoreEngine(
			vmbo,
			vmro,
			controllers.pvcs.Cache(),
			controllers.pvcs,
			controllers.vss.Cache(),
		),
	}
}

// newRestoreHandler creates a new RestoreHandler with all dependencies
func newRestoreHandler(
	ctx context.Context,
	controllers *restoreControllerSet,
	vmbo backupcommon.VMBackupOperator,
	vmro restorecommon.VMRestoreOperator,
	engines map[harvesterv1.BackupType]engine.RestoreEngine,
) *RestoreHandler {
	return &RestoreHandler{
		context:       ctx,
		vmrController: controllers.vmrs,
		vmrCache:      controllers.vmrs.Cache(),
		vmClient:      controllers.vms,
		vscClient:     controllers.vscs,
		vscCache:      controllers.vscs.Cache(),
		volumeCache:   controllers.volumes.Cache(),
		volumes:       controllers.volumes,
		scCache:       controllers.scs.Cache(),
		vmbo:          vmbo,
		vmro:          vmro,
		engines:       engines,
	}
}

// registerRestoreEventHandlers registers all event handlers for the restore controller
func registerRestoreEventHandlers(ctx context.Context, controllers *restoreControllerSet, handler *RestoreHandler) {
	controllers.vmrs.OnChange(ctx, restoreControllerName, handler.RestoreOnChanged)
	controllers.vmrs.OnRemove(ctx, restoreControllerName, handler.RestoreOnRemove)
	controllers.pvcs.OnChange(ctx, restoreControllerName, handler.PersistentVolumeClaimOnChange)
	controllers.lhengines.OnChange(ctx, restoreControllerName, handler.LHEngineOnChange)
	controllers.vms.OnChange(ctx, restoreControllerName, handler.VMOnChange)
}

// getRestoreEngine selects the appropriate restore engine based on backup type
func (h *RestoreHandler) getRestoreEngine(backup *harvesterv1.VirtualMachineBackup) engine.RestoreEngine {
	if engine, exists := h.engines[backup.Spec.Type]; exists {
		return engine
	}
	return nil
}

// RestoreOnChanged handles vmRestore CRD object on change, it will help to create the new PVCs and either replace them
// with existing VM or used for the new VM.
func (h *RestoreHandler) RestoreOnChanged(_ string, vmr *harvesterv1.VirtualMachineRestore) (*harvesterv1.VirtualMachineRestore, error) {
	if vmr == nil || h.vmro.IsDeleting(vmr) {
		return nil, nil
	}

	if !h.vmro.IsProgressing(vmr) {
		return nil, nil
	}

	if !h.vmro.HasStatus(vmr) {
		return nil, h.vmro.InitVMRestore(vmr)
	}

	vmb, err := h.vmro.GetVMBackup(vmr)
	if err != nil {
		return nil, h.vmro.UpdateError(vmr, err)
	}

	if h.vmro.IsMissingVolumes(vmr) {
		return nil, h.vmro.InitVolumesStatus(vmr, vmb)
	}

	vm, isVolumesReady, err := h.reconcileResources(vmr, vmb)
	if err != nil {
		return nil, h.vmro.UpdateError(vmr, err)
	}

	// set vmRestore owner reference to the target VM
	if !h.vmro.HasOwnerReference(vmr) {
		return nil, h.vmro.UpdateOwnerRefAndTargetUID(vmr, vm)
	}

	return nil, h.updateStatus(vmr, vmb, vm, isVolumesReady)
}

// RestoreOnRemove delegates per-volume cleanup to the restore engine, mirroring
// the (engine, iterate volumes) pattern in reconcileVolumeRestores.
func (h *RestoreHandler) RestoreOnRemove(_ string, vmr *harvesterv1.VirtualMachineRestore) (*harvesterv1.VirtualMachineRestore, error) {
	if vmr == nil || !h.vmro.HasStatus(vmr) {
		return nil, nil
	}

	re, err := h.resolveRestoreEngine(vmr)
	if err != nil {
		return nil, err
	}
	if re == nil {
		return nil, nil
	}

	for i := range h.vmro.GetVolRestores(vmr) {
		if err := re.Delete(vmr, i); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// resolveRestoreEngine picks the restore engine for a VMRestore. When the
// source VMBackup is gone (typically because the user deleted it ahead of the
// restore) we fall back to the Longhorn engine — the only engine with
// non-trivial cleanup (VolumeSnapshotContent is cluster-scoped, uses Retain
// policy, so it can't be garbage-collected via owner references).
func (h *RestoreHandler) resolveRestoreEngine(vmr *harvesterv1.VirtualMachineRestore) (engine.RestoreEngine, error) {
	vmb, err := h.vmro.GetVMBackup(vmr)
	if apierrors.IsNotFound(err) {
		return h.engines[harvesterv1.Backup], nil
	}
	if err != nil {
		return nil, err
	}
	return h.getRestoreEngine(vmb), nil
}

// pv naming convention from externel-provisioner
// porting from https://github.com/kubernetes-csi/external-provisioner/blob/90eae32d3a7352590500073b72b9a07f43adc881/pkg/controller/controller.go#L420-L436
func makeVolumeName(prefix, pvcUID string, volumeNameUUIDLength int) (string, error) {
	// create persistent name based on a volumeNamePrefix and volumeNameUUIDLength
	// of PVC's UID
	if len(prefix) == 0 {
		return "", fmt.Errorf("volume name prefix cannot be of length 0")
	}
	if len(pvcUID) == 0 {
		return "", fmt.Errorf("corrupted PVC object, it is missing UID")
	}
	if volumeNameUUIDLength == -1 {
		// Default behavior is to not truncate or remove dashes
		return fmt.Sprintf("%s-%s", prefix, pvcUID), nil
	}
	// Else we remove all dashes from UUID and truncate to volumeNameUUIDLength
	return fmt.Sprintf("%s-%s", prefix, strings.ReplaceAll(string(pvcUID), "-", "")[0:volumeNameUUIDLength]), nil

}

func getVolumeName(pvc *corev1.PersistentVolumeClaim) (string, error) {
	volumeName, err := makeVolumeName(pvNamePrefix, string(pvc.ObjectMeta.UID), volumeNameUUIDNoTruncate)
	if err != nil {
		return "", err
	}

	//sync with LH's naming convention on volume
	//https://github.com/longhorn/longhorn-manager/blob/88c792f7df38383634c2c8401f96d999385458c1/csi/controller_server.go#L83
	volumeName = lhutil.AutoCorrectName(volumeName, lhdatastore.NameMaximumLength)
	return volumeName, err
}

func (h *RestoreHandler) checkLHNotVolumeExist(pvc *corev1.PersistentVolumeClaim, restore string) (*corev1.PersistentVolumeClaim, error) {
	provisioner := util.GetProvisionedPVCProvisioner(pvc, h.scCache)
	if provisioner == types.LonghornDriverName {
		return nil, fmt.Errorf("LH pvc %s/%s missing volume", pvc.Namespace, pvc.Name)
	}

	// The storage provider is not LH, we should enqueue vmrestore
	logrus.WithFields(logrus.Fields{
		"namespace": pvc.Namespace,
		"name":      pvc.Name,
	}).Info("Non-LH PVC updating")

	h.vmrController.Enqueue(pvc.Namespace, restore)
	return nil, nil
}

// PersistentVolumeClaimOnChange watching the PVCs on change and enqueue the vmRestore if it has the restore annotation
func (h *RestoreHandler) PersistentVolumeClaimOnChange(_ string, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	if pvc == nil || pvc.DeletionTimestamp != nil {
		return nil, nil
	}

	restoreName, ok := pvc.Annotations[restoreNameAnnotation]
	if !ok {
		return nil, nil
	}

	volumeName, err := getVolumeName(pvc)
	if err != nil {
		return nil, err
	}

	volume, err := h.volumeCache.Get(util.LonghornSystemNamespaceName, volumeName)
	if apierrors.IsNotFound(err) {
		return h.checkLHNotVolumeExist(pvc, restoreName)
	}

	if err != nil {
		return nil, err
	}

	volumeCopy := volume.DeepCopy()
	if volumeCopy.Annotations == nil {
		volumeCopy.Annotations = make(map[string]string)
	}

	volumeCopy.Annotations[pvcNameSpaceAnnotation] = pvc.Namespace
	volumeCopy.Annotations[pvcNameAnnotation] = pvc.Name
	volumeCopy.Annotations[restoreNameAnnotation] = restoreName

	if !reflect.DeepEqual(volume, volumeCopy) {
		if _, err := h.volumes.Update(volumeCopy); err != nil {
			return nil, err
		}
	}

	logrus.Debugf("handling PVC updating %s/%s", pvc.Namespace, pvc.Name)
	h.vmrController.EnqueueAfter(pvc.Namespace, restoreName, 5*time.Second)
	return nil, nil
}

func (h *RestoreHandler) LHEngineOnChange(_ string, lhEngine *lhv1beta2.Engine) (*lhv1beta2.Engine, error) {
	if !h.shouldProcessEngine(lhEngine) {
		return nil, nil
	}

	pvcNamespace, pvcName, restoreName, volumeSize, err := h.getVolumeRestoreInfo(lhEngine)
	if err != nil {
		return nil, err
	}

	if pvcNamespace == "" {
		return nil, nil
	}

	vmr, err := h.vmrCache.Get(pvcNamespace, restoreName)
	if err != nil {
		return nil, nil
	}

	if err := h.updateVolumeRestoreMetrics(vmr, pvcNamespace, pvcName, volumeSize, lhEngine); err != nil {
		return nil, err
	}

	h.vmrController.Enqueue(pvcNamespace, restoreName)
	return nil, nil
}

// shouldProcessEngine checks if the engine should be processed for restore updates
func (h *RestoreHandler) shouldProcessEngine(lhEngine *lhv1beta2.Engine) bool {
	return lhEngine != nil && lhEngine.DeletionTimestamp == nil && len(lhEngine.Status.RestoreStatus) > 0
}

// getVolumeRestoreInfo extracts volume restore information from the engine's associated volume
func (h *RestoreHandler) getVolumeRestoreInfo(lhEngine *lhv1beta2.Engine) (pvcNamespace, pvcName, restoreName string, volumeSize int64, err error) {
	volume, err := h.volumeCache.Get(util.LonghornSystemNamespaceName, lhEngine.Spec.VolumeName)
	if err != nil {
		return "", "", "", 0, err
	}

	pvcNamespace, ok := volume.Annotations[pvcNameSpaceAnnotation]
	if !ok {
		return "", "", "", 0, nil
	}

	pvcName, ok = volume.Annotations[pvcNameAnnotation]
	if !ok {
		return "", "", "", 0, nil
	}

	restoreName, ok = volume.Annotations[restoreNameAnnotation]
	if !ok {
		return "", "", "", 0, nil
	}

	return pvcNamespace, pvcName, restoreName, volume.Spec.Size, nil
}

// updateVolumeRestoreMetrics updates the volume restore with engine name and volume size
func (h *RestoreHandler) updateVolumeRestoreMetrics(
	vmr *harvesterv1.VirtualMachineRestore,
	pvcNamespace, pvcName string,
	volumeSize int64,
	lhEngine *lhv1beta2.Engine,
) error {
	vmrCpy := vmr.DeepCopy()

	vr := h.findMatchingVolumeRestore(vmrCpy, pvcNamespace, pvcName)
	if vr == nil {
		return nil
	}

	h.vmro.SetVolRestoreLHEngineName(vr, lhEngine.Name)
	h.vmro.SetVolRestoreVolumeSize(vr, volumeSize)

	_, err := h.vmro.UpdateByStatus(vmr, vmrCpy)
	return err
}

// findMatchingVolumeRestore finds the volume restore that matches the PVC namespace and name
func (h *RestoreHandler) findMatchingVolumeRestore(
	vmrCpy *harvesterv1.VirtualMachineRestore,
	pvcNamespace, pvcName string,
) *harvesterv1.VolumeRestore {
	vrs := h.vmro.GetVolRestores(vmrCpy)

	for i := range vrs {
		vr := h.vmro.GetVolRestore(vmrCpy, i)

		if h.vmro.GetVolRestorePVCNamespace(vr) == pvcNamespace &&
			h.vmro.GetVolRestorePVCName(vr) == pvcName {
			return vr
		}
	}

	return nil
}

// VMOnChange watching the VM on change and enqueue the vmRestore if it has the restore annotation
func (h *RestoreHandler) VMOnChange(_ string, vm *kubevirtv1.VirtualMachine) (*kubevirtv1.VirtualMachine, error) {
	if vm == nil || vm.DeletionTimestamp != nil {
		return nil, nil
	}

	restoreName, ok := vm.Annotations[restoreNameAnnotation]
	if !ok {
		return nil, nil
	}

	logrus.Debugf("handling VM updating %s/%s", vm.Namespace, vm.Name)
	h.vmrController.EnqueueAfter(vm.Namespace, restoreName, 5*time.Second)
	return nil, nil
}

func (h *RestoreHandler) reconcileResources(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
) (*kubevirtv1.VirtualMachine, bool, error) {
	// reconcile restoring volumes and create new PVC from CSI volumeSnapshot if not exist
	isVolumesReady, err := h.reconcileVolumeRestores(vmr, vmb)
	if err != nil {
		return nil, false, err
	}

	// reconcile VM
	vm, err := h.reconcileVM(vmr, vmb)
	if err != nil {
		return nil, false, err
	}

	//restore referenced secrets
	if err := h.vmro.RestoreSecrets(vmr, vmb, vm); err != nil {
		return nil, false, err
	}

	return vm, isVolumesReady, nil
}

func (h *RestoreHandler) reconcileVolumeRestores(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
) (bool, error) {
	// Select appropriate engine based on backup type
	re := h.getRestoreEngine(vmb)
	isVolumesReady := true

	// Reconcile each per-volume restore via the engine
	volRestores := h.vmro.GetVolRestores(vmr)
	for i := range volRestores {
		err := re.Reconcile(vmr, vmb, i)
		if err == nil {
			continue
		}

		if err == engine.ErrRetryLater {
			isVolumesReady = false
			continue
		}
		return false, err
	}

	return isVolumesReady, nil
}

func (h *RestoreHandler) reconcileVM(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
) (*kubevirtv1.VirtualMachine, error) {
	vm, err := h.vmro.GetTargetVM(vmr)
	if err != nil {
		return nil, err
	}

	if vm == nil {
		return h.createNewVM(vmr, vmb)
	}

	if h.isVMAlreadyRestored(vm, vmr) {
		return vm, nil
	}

	return h.updateExistingVM(vm, vmr, vmb)
}

// isVMAlreadyRestored checks if the VM has already been restored with the current restore ID
func (h *RestoreHandler) isVMAlreadyRestored(vm *kubevirtv1.VirtualMachine, vmr *harvesterv1.VirtualMachineRestore) bool {
	restoreID := h.vmro.GetRestoreID(vmr)
	lastRestoreID, ok := vm.Annotations[lastRestoreAnnotation]
	return ok && lastRestoreID == restoreID
}

// updateExistingVM updates an existing VM with the restore spec and volumes
func (h *RestoreHandler) updateExistingVM(
	vm *kubevirtv1.VirtualMachine,
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
) (*kubevirtv1.VirtualMachine, error) {
	sourceSpec := h.vmbo.GetSourceSpec(vmb)
	newVolumes, err := h.vmro.MapVolumesToRestoredPVCs(vmr, &sourceSpec.Spec)
	if err != nil {
		return nil, err
	}

	vmCpy := vm.DeepCopy()
	vmCpy.Spec = sourceSpec.Spec

	// if the source runStrategy is RerunOnFailure, Kubevirt will not start the new VMI
	// set the VM runStrategy as Halted, VMI will be kicked off in startVM()
	haltedRunStrategy := kubevirtv1.RunStrategyHalted
	vmCpy.Spec.RunStrategy = &haltedRunStrategy
	vmCpy.Spec.Template.Spec.Volumes = newVolumes

	h.setRestoreAnnotations(vmCpy, vmr)

	return h.vmClient.Update(vmCpy)
}

// setRestoreAnnotations sets the required restore annotations on the VM
func (h *RestoreHandler) setRestoreAnnotations(vm *kubevirtv1.VirtualMachine, vmr *harvesterv1.VirtualMachineRestore) {
	if vm.Annotations == nil {
		vm.Annotations = make(map[string]string)
	}
	vm.Annotations[lastRestoreAnnotation] = h.vmro.GetRestoreID(vmr)
	vm.Annotations[restoreNameAnnotation] = h.vmro.GetName(vmr)
	delete(vm.Annotations, util.AnnotationVolumeClaimTemplates)
}

func (h *RestoreHandler) createNewVM(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
) (*kubevirtv1.VirtualMachine, error) {
	targetName := h.vmro.GetTargetName(vmr)
	namespace := h.vmro.GetNamespace(vmr)
	keepMacAddress := h.vmro.IsKeepMacAddress(vmr)

	logrus.Infof("restore target does not exist, creating a new vm %s", targetName)

	sourceSpec := h.vmbo.GetSourceSpec(vmb)
	vm, err := h.buildVMFromRestore(vmr, sourceSpec, targetName, namespace)
	if err != nil {
		return nil, err
	}

	if !keepMacAddress {
		h.removeMacAddresses(vm)
	}

	return h.vmClient.Create(vm)
}

// buildVMFromRestore constructs a new VM object from restore and backup specs
func (h *RestoreHandler) buildVMFromRestore(
	vmr *harvesterv1.VirtualMachineRestore,
	sourceSpec *harvesterv1.VirtualMachineSourceSpec,
	targetName, namespace string,
) (*kubevirtv1.VirtualMachine, error) {
	vmAnnotations := h.buildVMAnnotations(vmr, sourceSpec)

	specAnnotations, err := h.buildVMSpecAnnotations(vmr, sourceSpec)
	if err != nil {
		return nil, err
	}

	runStrategy := h.getDefaultRunStrategy(sourceSpec)

	vm := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        targetName,
			Namespace:   namespace,
			Annotations: vmAnnotations,
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &runStrategy,
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: specAnnotations,
					Labels: map[string]string{
						util.LabelVMCreator: "harvester",
						util.LabelVMName:    targetName,
					},
				},
				Spec: h.vmro.SanitizeVMSpec(vmr, sourceSpec.Spec.Template.Spec),
			},
		},
	}

	newVolumes, err := h.vmro.MapVolumesToRestoredPVCs(vmr, &vm.Spec)
	if err != nil {
		return nil, err
	}
	vm.Spec.Template.Spec.Volumes = newVolumes

	return vm, nil
}

// buildVMAnnotations creates annotations for the new VM
func (h *RestoreHandler) buildVMAnnotations(
	vmr *harvesterv1.VirtualMachineRestore,
	sourceSpec *harvesterv1.VirtualMachineSourceSpec,
) map[string]string {
	restoreID := h.vmro.GetRestoreID(vmr)
	restoreName := h.vmro.GetName(vmr)

	annotations := map[string]string{
		lastRestoreAnnotation: restoreID,
		restoreNameAnnotation: restoreName,
	}

	// Preserve specific annotations from source VM
	preservedAnnotations := []string{
		util.AnnotationReservedMemory,
		util.AnnotationEnableCPUAndMemoryHotplug,
	}

	for _, annotation := range preservedAnnotations {
		if value, ok := sourceSpec.ObjectMeta.Annotations[annotation]; ok {
			annotations[annotation] = value
		}
	}

	return annotations
}

// buildVMSpecAnnotations creates annotations for the VM spec template
func (h *RestoreHandler) buildVMSpecAnnotations(
	vmr *harvesterv1.VirtualMachineRestore,
	sourceSpec *harvesterv1.VirtualMachineSourceSpec,
) (map[string]string, error) {
	return h.vmro.SanitizeVMAnnotations(vmr, sourceSpec.Spec.Template.ObjectMeta.Annotations)
}

// getDefaultRunStrategy determines the run strategy for the new VM
func (h *RestoreHandler) getDefaultRunStrategy(sourceSpec *harvesterv1.VirtualMachineSourceSpec) kubevirtv1.VirtualMachineRunStrategy {
	if sourceSpec.Spec.RunStrategy != nil {
		return *sourceSpec.Spec.RunStrategy
	}
	return kubevirtv1.RunStrategyRerunOnFailure
}

// removeMacAddresses removes MAC addresses from all network interfaces
func (h *RestoreHandler) removeMacAddresses(vm *kubevirtv1.VirtualMachine) {
	for i := range vm.Spec.Template.Spec.Domain.Devices.Interfaces {
		vm.Spec.Template.Spec.Domain.Devices.Interfaces[i].MacAddress = ""
	}
}

func (h *RestoreHandler) updateStatus(
	vmr *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
	vm *kubevirtv1.VirtualMachine,
	isVolumesReady bool,
) error {
	vmrCpy := vmr.DeepCopy()

	// 1. Calculate and update progress metrics
	if err := h.updateProgressMetrics(vmrCpy, vmb); err != nil {
		return err
	}

	// 2. Wait for volumes if not ready
	if !isVolumesReady {
		return h.handleVolumesNotReady(vmr, vmrCpy)
	}

	// 3. Ensure VM is started and ready
	if err := h.ensureVMStartedAndReady(vmr, vmrCpy, vm); err != nil {
		return err
	}

	// 4. Cleanup and complete
	return h.finalizeRestore(vmr, vmrCpy, vm)
}

// updateProgressMetrics calculates and updates the restore progress based on volume restore status
func (h *RestoreHandler) updateProgressMetrics(
	vmrCpy *harvesterv1.VirtualMachineRestore,
	vmb *harvesterv1.VirtualMachineBackup,
) error {
	re := h.getRestoreEngine(vmb)
	if re == nil {
		return fmt.Errorf("unsupported backup type: %s", vmb.Spec.Type)
	}

	// Update progress for each volume restore
	vrs := h.vmro.GetVolRestores(vmrCpy)
	for i := range vrs {
		vr := h.vmro.GetVolRestore(vmrCpy, i)
		progress, err := re.UpdateProgress(vr)
		if err != nil {
			return err
		}
		h.vmro.SetVolRestoreProgress(vr, int(progress))
	}

	return nil
}

// handleVolumesNotReady persists the "Creating new PVCs" progressing condition
// and returns a non-nil error so the reconciler requeues. Without the error
// return we would depend solely on PVC/Engine events to re-trigger reconcile,
// which is brittle.
func (h *RestoreHandler) handleVolumesNotReady(
	vmr *harvesterv1.VirtualMachineRestore,
	vmrCpy *harvesterv1.VirtualMachineRestore,
) error {
	h.vmro.RectifyProgressBeforeVMStart(vmrCpy)
	vmrCpy = h.vmro.SetProcessingCondition(vmrCpy, "Creating new PVCs")
	if _, err := h.vmro.Update(vmr, vmrCpy); err != nil {
		return err
	}
	return fmt.Errorf("volumes for vmrestore %s/%s are not ready yet", vmr.Namespace, vmr.Name)
}

// ensureVMStartedAndReady starts the VM if needed and waits for it to be ready
func (h *RestoreHandler) ensureVMStartedAndReady(
	vmr *harvesterv1.VirtualMachineRestore,
	vmrCpy *harvesterv1.VirtualMachineRestore,
	vm *kubevirtv1.VirtualMachine,
) error {
	// Start VM before checking status
	if err := h.vmro.StartVM(h.context, vm); err != nil {
		return h.vmro.UpdateError(vmr, fmt.Errorf("failed to start vm, err:%s", err.Error()))
	}

	if vm.Status.Ready {
		return nil
	}

	// VM not ready yet: persist progressing condition and halt the pipeline so
	// updateStatus does NOT fall through to finalizeRestore. The VMOnChange
	// handler will requeue the restore once the VM transitions to Ready.
	h.vmro.RectifyProgressBeforeVMStart(vmrCpy)
	vmrCpy = h.vmro.SetProcessingCondition(vmrCpy, "Waiting for target vm to be ready")
	if _, err := h.vmro.Update(vmr, vmrCpy); err != nil {
		return err
	}
	return fmt.Errorf("vm %s/%s is not ready yet", vm.Namespace, vm.Name)

}

// finalizeRestore performs cleanup and marks the restore as complete
func (h *RestoreHandler) finalizeRestore(
	vmr *harvesterv1.VirtualMachineRestore,
	vmrCpy *harvesterv1.VirtualMachineRestore,
	vm *kubevirtv1.VirtualMachine,
) error {
	if err := h.vmro.DeleteOldPVCs(vmrCpy, vm); err != nil {
		return h.vmro.UpdateError(vmr, fmt.Errorf("error cleaning up: %w", err))
	}
	vmrCpy = h.vmro.SetCompleteCondition(vmrCpy)
	_, err := h.vmro.Update(vmr, vmrCpy)
	return err
}

