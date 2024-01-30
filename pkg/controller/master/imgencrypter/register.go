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

	imgEncrypterHandler := &imgEncrypterHandler{
		encrypters:          encrypters,
		encrypterController: encrypters,
	}

	encrypters.OnChange(ctx, imgEncrypterControllerName, imgEncrypterHandler.OnChanged)
	encrypters.OnRemove(ctx, imgEncrypterControllerName, imgEncrypterHandler.OnRemove)
	return nil
}
