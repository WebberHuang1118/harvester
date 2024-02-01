package imgencrypter

import (
	"fmt"

	ctlcorev1 "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/pkg/generated/controllers/storage/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
)

const (
	cryptoSC   = "longhorn-migratable"
	srcPVCName = "src-pvc"
	srcPVCSize = "5Gi"
	dstPVCName = "dst-pvc"
	dstPVCSize = "6Gi"
)

type imgEncrypterHandler struct {
	encrypterController ctlharvesterv1.ImgEncrypterController
	encrypterClient     ctlharvesterv1.ImgEncrypterClient
	pvcClient           ctlcorev1.PersistentVolumeClaimClient
	pvcCache            ctlcorev1.PersistentVolumeClaimCache
	imageCache          ctlharvesterv1.VirtualMachineImageCache
	storageClassCache   ctlstoragev1.StorageClassCache
}

func (h *imgEncrypterHandler) createSrcPVC(encrypter *harvesterv1.ImgEncrypter) (*corev1.PersistentVolumeClaim, error) {
	image, err := h.imageCache.Get(encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName)
	if err != nil {
		logrus.Infof("get image %v/%v fail with err %v",
			encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName, err)
		return nil, err
	}

	volumeMode := corev1.PersistentVolumeBlock
	storageClassName := image.Status.StorageClassName
	srcPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: encrypter.Namespace,
			Name:      srcPVCName,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"storage": resource.MustParse(srcPVCSize),
				},
			},
			VolumeMode:       &volumeMode,
			StorageClassName: &storageClassName,
		},
	}

	return h.pvcClient.Create(srcPVC)
}

func (h *imgEncrypterHandler) checkSrcPVC(encrypter *harvesterv1.ImgEncrypter) (*corev1.PersistentVolumeClaim, error) {
	srcPVC, err := h.pvcCache.Get(encrypter.Namespace, srcPVCName)
	if apierrors.IsNotFound(err) {
		if _, err := h.createSrcPVC(encrypter); err != nil {
			logrus.Infof("checkSrcPVC: createSrcPVC fail with err %v", err)
			return nil, err
		}
		return nil, fmt.Errorf("checking src pvc again ")
	}

	if err != nil {
		return nil, err
	}

	if srcPVC.Status.Phase == corev1.ClaimPending {
		logrus.Infof("checkSrcPVC: srcPVC in pending")
		return nil, fmt.Errorf("src pvc in pending")
	}

	return srcPVC, nil
}

func (h *imgEncrypterHandler) createDstPVC(encrypter *harvesterv1.ImgEncrypter) (*corev1.PersistentVolumeClaim, error) {
	sc, err := h.storageClassCache.Get(cryptoSC)
	if err != nil {
		logrus.Infof("get sc %v fail with err %v", cryptoSC, err)
		return nil, err
	}

	volumeMode := corev1.PersistentVolumeBlock
	dstPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: encrypter.Namespace,
			Name:      dstPVCName,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"storage": resource.MustParse(dstPVCSize),
				},
			},
			VolumeMode:       &volumeMode,
			StorageClassName: &sc.Name,
		},
	}

	return h.pvcClient.Create(dstPVC)
}

func (h *imgEncrypterHandler) checkDstPVC(encrypter *harvesterv1.ImgEncrypter) (*corev1.PersistentVolumeClaim, error) {
	dstPVC, err := h.pvcCache.Get(encrypter.Namespace, dstPVCName)
	if apierrors.IsNotFound(err) {
		if _, err := h.createDstPVC(encrypter); err != nil {
			logrus.Infof("checkDstPVC: createDstPVC fail with err %v", err)
			return nil, err
		}
		return nil, fmt.Errorf("checking dst pvc again ")
	}

	if err != nil {
		return nil, err
	}

	if dstPVC.Status.Phase == corev1.ClaimPending {
		logrus.Infof("checkDstPVC: dstPVC in pending")
		return nil, fmt.Errorf("dst pvc in pending")
	}

	return dstPVC, nil
}

func (h *imgEncrypterHandler) OnChanged(_ string, encrypter *harvesterv1.ImgEncrypter) (*harvesterv1.ImgEncrypter, error) {
	if encrypter == nil || encrypter.DeletionTimestamp != nil {
		return encrypter, nil
	}

	var stage int64
	toUpdate := encrypter.DeepCopy()

	defer func() {
		if stage <= toUpdate.Status.Stage {
			return
		}

		toUpdate.Status.Stage = stage
		h.encrypterController.Update(toUpdate)
	}()

	logrus.Infof("reconcile imgEncrypter %v for img %s/%s", encrypter.Name,
		encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName)

	_, err := h.checkSrcPVC(encrypter)
	if err != nil {
		return nil, err
	}
	stage = 1

	_, err = h.checkDstPVC(encrypter)
	if err != nil {
		return nil, err
	}
	stage = 2

	return encrypter, nil
}

func (h *imgEncrypterHandler) OnRemove(_ string, encrypter *harvesterv1.ImgEncrypter) (*harvesterv1.ImgEncrypter, error) {
	if encrypter == nil {
		return nil, nil
	}

	logrus.Infof("OnRemove imgEncrypter %v for img %s/%s", encrypter.Name,
		encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName)

	return encrypter, nil
}
