package imgencrypter

import (
	"fmt"

	lhv1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	catalogv1 "github.com/rancher/rancher/pkg/generated/controllers/catalog.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/condition"
	ctlbatchv1 "github.com/rancher/wrangler/pkg/generated/controllers/batch/v1"
	ctlcorev1 "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	ctlstoragev1 "github.com/rancher/wrangler/pkg/generated/controllers/storage/v1"
	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
	ctllhv1 "github.com/harvester/harvester/pkg/generated/controllers/longhorn.io/v1beta2"
	"github.com/harvester/harvester/pkg/util"
	utilCatalog "github.com/harvester/harvester/pkg/util/catalog"
)

const (
	cryptoSC                = "longhorn-migratable"
	srcPVCName              = "src-pvc"
	srcPVCSize              = "5Gi"
	srcVolName              = "src-vol"
	srcVolDevPath           = "/dev/src-vol"
	dstPVCName              = "dst-pvc"
	dstPVCSize              = "6Gi"
	dstVolName              = "dst-vol"
	dstVolDevPath           = "/dev/dst-vol"
	transferJob             = "transfer-job"
	releaseAppHarvesterName = "harvester"
	dataCopyCmd             = "cp"
	encryptImgPrefix        = "encrypt-"
)

var (
	ConditionJobComplete = condition.Cond(batchv1.JobComplete)
	ConditionJobFailed   = condition.Cond(batchv1.JobFailed)

	ConditionImgInitialized = condition.Cond(harvesterv1.ImageInitialized)
	ConditionImgImported    = condition.Cond(harvesterv1.ImageImported)
	ConditionImgRetryExceed = condition.Cond(harvesterv1.ImageRetryLimitExceeded)
)

type imgEncrypterHandler struct {
	encrypterController ctlharvesterv1.ImgEncrypterController
	encrypterClient     ctlharvesterv1.ImgEncrypterClient
	pvcClient           ctlcorev1.PersistentVolumeClaimClient
	pvcCache            ctlcorev1.PersistentVolumeClaimCache
	imageClient         ctlharvesterv1.VirtualMachineImageClient
	imageCache          ctlharvesterv1.VirtualMachineImageCache
	storageClassCache   ctlstoragev1.StorageClassCache
	namespace           string
	appCache            catalogv1.AppCache
	jobs                ctlbatchv1.JobClient
	jobCache            ctlbatchv1.JobCache
	volumeCache         ctllhv1.VolumeCache
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

func (h *imgEncrypterHandler) createJob(encrypter *harvesterv1.ImgEncrypter, srcPVC string, dstPVC string) (*batchv1.Job, error) {
	jobImage, err := utilCatalog.FetchAppChartImage(h.appCache, h.namespace, releaseAppHarvesterName, []string{"generalJob", "image"})
	if err != nil {
		return nil, fmt.Errorf("failed to get harvester image (%s): %v", jobImage.ImageName(), err)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      transferJob,
			Namespace: encrypter.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{{
						Name: srcVolName,
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: srcPVC,
							},
						},
					}, {
						Name: dstVolName,
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: dstPVC,
							},
						},
					}},
					ServiceAccountName: encrypter.Namespace,
				},
			},
		},
	}
	podTemplate := &job.Spec.Template

	podTemplate.Spec.Containers = []corev1.Container{
		{
			Name:    transferJob,
			Image:   jobImage.ImageName(),
			Command: []string{dataCopyCmd},
			Args: []string{
				srcVolDevPath,
				dstVolDevPath,
			},
			Resources: corev1.ResourceRequirements{},
			VolumeDevices: []corev1.VolumeDevice{
				{Name: srcVolName, DevicePath: srcVolDevPath},
				{Name: dstVolName, DevicePath: dstVolDevPath},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &corev1.SecurityContext{
				Privileged: pointer.Bool(true),
			},
		},
	}

	return h.jobs.Create(job)
}

func (h *imgEncrypterHandler) checkJob(encrypter *harvesterv1.ImgEncrypter, srcPVC string, dstPVC string) (*batchv1.Job, error) {
	job, err := h.jobCache.Get(encrypter.Namespace, transferJob)
	if apierrors.IsNotFound(err) {
		if _, err := h.createJob(encrypter, srcPVC, dstPVC); err != nil {
			logrus.Infof("checkJob: createJob fail with err %v", err)
			return nil, err
		}
		return nil, fmt.Errorf("checking transfer job again")
	}

	if err != nil {
		return nil, err
	}

	if ConditionJobFailed.IsTrue(job) {
		return nil, fmt.Errorf("transfer job failed")
	}

	if !ConditionJobComplete.IsTrue(job) {
		return nil, fmt.Errorf("transfer job not complete yet")
	}

	return job, nil
}

func getVolumeName(pvc *corev1.PersistentVolumeClaim) (string, error) {
	if pvc.Status.Phase == corev1.ClaimPending {
		return "", fmt.Errorf("pvc %s in pending", pvc.Name)
	}

	return pvc.Spec.VolumeName, nil
}

func (h *imgEncrypterHandler) checkPVCDetached(encrypter *harvesterv1.ImgEncrypter, pvc *corev1.PersistentVolumeClaim) error {
	volumeName, err := getVolumeName(pvc)
	if err != nil {
		return err
	}

	volume, err := h.volumeCache.Get(util.LonghornSystemNamespaceName, volumeName)
	if err != nil {
		return err
	}

	if volume.Status.State != lhv1beta2.VolumeStateDetached {
		return fmt.Errorf("volume %s not detached yet", volumeName)
	}

	return nil
}

func (h *imgEncrypterHandler) exportImage(encrypter *harvesterv1.ImgEncrypter, srcImg *harvesterv1.VirtualMachineImage) (*harvesterv1.VirtualMachineImage, error) {
	img := &harvesterv1.VirtualMachineImage{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: encrypter.Namespace,
			Name:      encryptImgPrefix + srcImg.Spec.DisplayName,
			Annotations: map[string]string{
				util.AnnotationStorageClassName: cryptoSC,
			},
		},
		Spec: harvesterv1.VirtualMachineImageSpec{
			DisplayName:  encryptImgPrefix + srcImg.Spec.DisplayName,
			SourceType:   harvesterv1.VirtualMachineImageSourceTypeExportVolume,
			PVCName:      dstPVCName,
			PVCNamespace: encrypter.Namespace,
		},
	}

	return h.imageClient.Create(img)
}

func (h *imgEncrypterHandler) checkImage(encrypter *harvesterv1.ImgEncrypter) (*harvesterv1.VirtualMachineImage, error) {
	srcImg, err := h.imageCache.Get(encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName)
	if err != nil {
		logrus.Infof("get src image %v/%v fail with err %v",
			encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName, err)
		return nil, err
	}

	img, err := h.imageCache.Get(encrypter.Namespace, encryptImgPrefix+srcImg.Spec.DisplayName)
	if apierrors.IsNotFound(err) {
		if _, err := h.exportImage(encrypter, srcImg); err != nil {
			logrus.Infof("checkImage: export image fail with err %v", err)
			return nil, err
		}
		return nil, fmt.Errorf("checking image again")
	}

	if err != nil {
		return nil, err
	}

	if ConditionImgInitialized.IsFalse(img) {
		return nil, fmt.Errorf("image initialize failed")
	}

	if ConditionImgRetryExceed.IsTrue(img) {
		return nil, fmt.Errorf("image reach retry exceed")
	}

	if ConditionImgImported.IsFalse(img) {
		return nil, fmt.Errorf("image not imported yet")
	}

	return img, nil
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

	srcPVC, err := h.checkSrcPVC(encrypter)
	if err != nil {
		return nil, err
	}
	stage = 1

	dstPVC, err := h.checkDstPVC(encrypter)
	if err != nil {
		return nil, err
	}
	stage = 2

	_, err = h.checkJob(encrypter, srcPVC.Name, dstPVC.Name)
	if err != nil {
		return nil, err
	}
	stage = 3

	if err := h.checkPVCDetached(encrypter, srcPVC); err != nil {
		return nil, err
	}
	stage = 4

	if err := h.checkPVCDetached(encrypter, dstPVC); err != nil {
		return nil, err
	}
	stage = 5

	if _, err := h.checkImage(encrypter); err != nil {
		return nil, err
	}
	stage = 6

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
