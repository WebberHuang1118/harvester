/*
Copyright 2024 Rancher Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by main. DO NOT EDIT.

package v1beta2

import (
	v1beta2 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/wrangler/pkg/schemes"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	schemes.Register(v1beta2.AddToScheme)
}

type Interface interface {
	BackingImage() BackingImageController
	BackingImageDataSource() BackingImageDataSourceController
	Backup() BackupController
	Engine() EngineController
	Replica() ReplicaController
	Setting() SettingController
	Volume() VolumeController
}

func New(controllerFactory controller.SharedControllerFactory) Interface {
	return &version{
		controllerFactory: controllerFactory,
	}
}

type version struct {
	controllerFactory controller.SharedControllerFactory
}

func (c *version) BackingImage() BackingImageController {
	return NewBackingImageController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "BackingImage"}, "backingimages", true, c.controllerFactory)
}
func (c *version) BackingImageDataSource() BackingImageDataSourceController {
	return NewBackingImageDataSourceController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "BackingImageDataSource"}, "backingimagedatasources", true, c.controllerFactory)
}
func (c *version) Backup() BackupController {
	return NewBackupController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Backup"}, "backups", true, c.controllerFactory)
}
func (c *version) Engine() EngineController {
	return NewEngineController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Engine"}, "engines", true, c.controllerFactory)
}
func (c *version) Replica() ReplicaController {
	return NewReplicaController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Replica"}, "replicas", true, c.controllerFactory)
}
func (c *version) Setting() SettingController {
	return NewSettingController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Setting"}, "settings", true, c.controllerFactory)
}
func (c *version) Volume() VolumeController {
	return NewVolumeController(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Volume"}, "volumes", true, c.controllerFactory)
}
