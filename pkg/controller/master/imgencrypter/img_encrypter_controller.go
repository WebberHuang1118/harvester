package imgencrypter

import (
	"time"

	"github.com/sirupsen/logrus"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
)

type imgEncrypterHandler struct {
	encrypters          ctlharvesterv1.ImgEncrypterClient
	encrypterController ctlharvesterv1.ImgEncrypterController
}

func (h *imgEncrypterHandler) OnChanged(_ string, encrypter *harvesterv1.ImgEncrypter) (*harvesterv1.ImgEncrypter, error) {
	if encrypter == nil || encrypter.DeletionTimestamp != nil {
		return encrypter, nil
	}

	logrus.Infof("reconcile imgEncrypter %v for img %s/%s", encrypter.Name,
		encrypter.Spec.SrcImgNamespace, encrypter.Spec.SrcImgName)

	h.encrypterController.EnqueueAfter(encrypter.Namespace, encrypter.Name, 10*time.Second)
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
