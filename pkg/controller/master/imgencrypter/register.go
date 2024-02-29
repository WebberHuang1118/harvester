package imgencrypter

import (
	"context"

	"github.com/harvester/harvester/pkg/config"
)

const (
	imgEncrypterControllerName = "img-encrypter-controller"
)

func Register(ctx context.Context, management *config.Management, options config.Options) error {
	encrypters := management.HarvesterFactory.Harvesterhci().V1beta1().ImgEncrypter()
	pvcs := management.CoreFactory.Core().V1().PersistentVolumeClaim()
	images := management.HarvesterFactory.Harvesterhci().V1beta1().VirtualMachineImage()
	storageClasses := management.StorageFactory.Storage().V1().StorageClass()
	appCache := management.CatalogFactory.Catalog().V1().App().Cache()
	jobs := management.BatchFactory.Batch().V1().Job()
	volumes := management.LonghornFactory.Longhorn().V1beta2().Volume()

	imgEncrypterHandler := &imgEncrypterHandler{
		encrypterController: encrypters,
		encrypterClient:     encrypters,
		pvcClient:           pvcs,
		pvcCache:            pvcs.Cache(),
		imageClient:         images,
		imageCache:          images.Cache(),
		storageClassCache:   storageClasses.Cache(),
		namespace:           options.Namespace,
		appCache:            appCache,
		jobs:                jobs,
		jobCache:            jobs.Cache(),
		volumeCache:         volumes.Cache(),
	}

	encrypters.OnChange(ctx, imgEncrypterControllerName, imgEncrypterHandler.OnChanged)
	encrypters.OnRemove(ctx, imgEncrypterControllerName, imgEncrypterHandler.OnRemove)
	return nil
}
