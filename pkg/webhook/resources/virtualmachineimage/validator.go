package virtualmachineimage

import (
	"reflect"

	ctlcorev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/storage/v1"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/image/backend"
	"github.com/harvester/harvester/pkg/image/backingimage"
	"github.com/harvester/harvester/pkg/image/cdi"
	"github.com/harvester/harvester/pkg/image/common"
	"github.com/harvester/harvester/pkg/util"
	"github.com/harvester/harvester/pkg/webhook/types"
)

func NewValidator(
	vmiCache ctlharvesterv1.VirtualMachineImageCache,
	podCache ctlcorev1.PodCache,
	pvcCache ctlcorev1.PersistentVolumeClaimCache,
	vmTemplateVersionCache ctlharvesterv1.VirtualMachineTemplateVersionCache,
	scCache ctlstoragev1.StorageClassCache,
	vmBackupCache ctlharvesterv1.VirtualMachineBackupCache) types.Validator {

	vmiv := common.GetVMIValidator(vmiCache, scCache, podCache, pvcCache, vmTemplateVersionCache, vmBackupCache)
	validators := map[v1beta1.VMIBackend]backend.Validator{
		v1beta1.VMIBackendBackingImage: backingimage.GetValidator(vmiv),
		v1beta1.VMIBackendCDI:          cdi.GetValidator(vmiv),
	}

	return &virtualMachineImageValidator{
		validators: validators,
	}
}

func (v *virtualMachineImageValidator) ResolveAccessChecks(
	_ *types.Request,
	operation admissionv1.Operation,
	oldObj runtime.Object,
	newObj runtime.Object,
) ([]types.RelatedResource, error) {
	vmi, _ := newObj.(*v1beta1.VirtualMachineImage)
	if vmi == nil {
		return nil, nil
	}

	resources := resolveVMIRefs(vmi)
	switch operation {
	case admissionv1.Create:
		return resources, nil
	case admissionv1.Update:
		oldVMI, _ := oldObj.(*v1beta1.VirtualMachineImage)
		if oldVMI != nil && reflect.DeepEqual(resolveVMIRefs(oldVMI), resources) {
			return nil, nil
		}
		return resources, nil
	default:
		return nil, nil
	}
}

func resolveVMIRefs(vmi *v1beta1.VirtualMachineImage) []types.RelatedResource {
	switch vmi.Spec.SourceType {
	case v1beta1.VirtualMachineImageSourceTypeClone:
		parameters := vmi.Spec.SecurityParameters
		if parameters == nil || parameters.CryptoOperation == "" ||
			parameters.SourceImageNamespace == "" || parameters.SourceImageName == "" {
			return nil
		}
		return []types.RelatedResource{{
			GVR:         util.VirtualMachineImageGVR,
			Namespace:   parameters.SourceImageNamespace,
			Name:        parameters.SourceImageName,
			Description: "source image",
		}}
	case v1beta1.VirtualMachineImageSourceTypeExportVolume:
		if vmi.Spec.PVCNamespace == "" || vmi.Spec.PVCName == "" {
			return nil
		}
		return []types.RelatedResource{{
			GVR:         util.PVCGVR,
			Namespace:   vmi.Spec.PVCNamespace,
			Name:        vmi.Spec.PVCName,
			Description: "PVC",
		}}
	default:
		return nil
	}
}

type virtualMachineImageValidator struct {
	types.DefaultValidator
	validators map[v1beta1.VMIBackend]backend.Validator
}

func (v *virtualMachineImageValidator) Resource() types.Resource {
	return types.Resource{
		Names:      []string{v1beta1.VirtualMachineImageResourceName},
		Scope:      admissionregv1.NamespacedScope,
		APIGroup:   v1beta1.SchemeGroupVersion.Group,
		APIVersion: v1beta1.SchemeGroupVersion.Version,
		ObjectType: &v1beta1.VirtualMachineImage{},
		OperationTypes: []admissionregv1.OperationType{
			admissionregv1.Create,
			admissionregv1.Update,
			admissionregv1.Delete,
		},
	}
}

func (v *virtualMachineImageValidator) Create(request *types.Request, newObj runtime.Object) error {
	vmi := newObj.(*v1beta1.VirtualMachineImage)
	return v.validators[util.GetVMIBackend(vmi)].Create(request, vmi)
}

func (v *virtualMachineImageValidator) Update(_ *types.Request, oldObj runtime.Object, newObj runtime.Object) error {
	newVMI := newObj.(*v1beta1.VirtualMachineImage)
	oldVMI := oldObj.(*v1beta1.VirtualMachineImage)
	return v.validators[util.GetVMIBackend(oldVMI)].Update(oldVMI, newVMI)
}

func (v *virtualMachineImageValidator) Delete(_ *types.Request, oldObj runtime.Object) error {
	vmi := oldObj.(*v1beta1.VirtualMachineImage)
	return v.validators[util.GetVMIBackend(vmi)].Delete(vmi)
}
