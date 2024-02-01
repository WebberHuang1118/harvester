package imgencrypter

import (
	"context"

	"github.com/harvester/harvester/pkg/config"
)

const (
	imgEncrypterControllerName = "img-encrypter-controller"
)

func Register(ctx context.Context, management *config.Management, _ config.Options) error {
	encrypters := management.HarvesterFactory.Harvesterhci().V1beta1().ImgEncrypter()
	pvcs := management.CoreFactory.Core().V1().PersistentVolumeClaim()
	images := management.HarvesterFactory.Harvesterhci().V1beta1().VirtualMachineImage()
	storageClasses := management.StorageFactory.Storage().V1().StorageClass()

	imgEncrypterHandler := &imgEncrypterHandler{
		encrypterController: encrypters,
		encrypterClient:     encrypters,
		pvcClient:           pvcs,
		pvcCache:            pvcs.Cache(),
		imageCache:          images.Cache(),
		storageClassCache:   storageClasses.Cache(),
	}

	encrypters.OnChange(ctx, imgEncrypterControllerName, imgEncrypterHandler.OnChanged)
	encrypters.OnRemove(ctx, imgEncrypterControllerName, imgEncrypterHandler.OnRemove)
	return nil
}
