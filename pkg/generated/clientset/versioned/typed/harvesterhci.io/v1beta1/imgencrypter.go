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

package v1beta1

import (
	"context"
	"time"

	v1beta1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	scheme "github.com/harvester/harvester/pkg/generated/clientset/versioned/scheme"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
)

// ImgEncryptersGetter has a method to return a ImgEncrypterInterface.
// A group's client should implement this interface.
type ImgEncryptersGetter interface {
	ImgEncrypters(namespace string) ImgEncrypterInterface
}

// ImgEncrypterInterface has methods to work with ImgEncrypter resources.
type ImgEncrypterInterface interface {
	Create(ctx context.Context, imgEncrypter *v1beta1.ImgEncrypter, opts v1.CreateOptions) (*v1beta1.ImgEncrypter, error)
	Update(ctx context.Context, imgEncrypter *v1beta1.ImgEncrypter, opts v1.UpdateOptions) (*v1beta1.ImgEncrypter, error)
	UpdateStatus(ctx context.Context, imgEncrypter *v1beta1.ImgEncrypter, opts v1.UpdateOptions) (*v1beta1.ImgEncrypter, error)
	Delete(ctx context.Context, name string, opts v1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error
	Get(ctx context.Context, name string, opts v1.GetOptions) (*v1beta1.ImgEncrypter, error)
	List(ctx context.Context, opts v1.ListOptions) (*v1beta1.ImgEncrypterList, error)
	Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1beta1.ImgEncrypter, err error)
	ImgEncrypterExpansion
}

// imgEncrypters implements ImgEncrypterInterface
type imgEncrypters struct {
	client rest.Interface
	ns     string
}

// newImgEncrypters returns a ImgEncrypters
func newImgEncrypters(c *HarvesterhciV1beta1Client, namespace string) *imgEncrypters {
	return &imgEncrypters{
		client: c.RESTClient(),
		ns:     namespace,
	}
}

// Get takes name of the imgEncrypter, and returns the corresponding imgEncrypter object, and an error if there is any.
func (c *imgEncrypters) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1beta1.ImgEncrypter, err error) {
	result = &v1beta1.ImgEncrypter{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("imgencrypters").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do(ctx).
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of ImgEncrypters that match those selectors.
func (c *imgEncrypters) List(ctx context.Context, opts v1.ListOptions) (result *v1beta1.ImgEncrypterList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1beta1.ImgEncrypterList{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("imgencrypters").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do(ctx).
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested imgEncrypters.
func (c *imgEncrypters) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Namespace(c.ns).
		Resource("imgencrypters").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch(ctx)
}

// Create takes the representation of a imgEncrypter and creates it.  Returns the server's representation of the imgEncrypter, and an error, if there is any.
func (c *imgEncrypters) Create(ctx context.Context, imgEncrypter *v1beta1.ImgEncrypter, opts v1.CreateOptions) (result *v1beta1.ImgEncrypter, err error) {
	result = &v1beta1.ImgEncrypter{}
	err = c.client.Post().
		Namespace(c.ns).
		Resource("imgencrypters").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(imgEncrypter).
		Do(ctx).
		Into(result)
	return
}

// Update takes the representation of a imgEncrypter and updates it. Returns the server's representation of the imgEncrypter, and an error, if there is any.
func (c *imgEncrypters) Update(ctx context.Context, imgEncrypter *v1beta1.ImgEncrypter, opts v1.UpdateOptions) (result *v1beta1.ImgEncrypter, err error) {
	result = &v1beta1.ImgEncrypter{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("imgencrypters").
		Name(imgEncrypter.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(imgEncrypter).
		Do(ctx).
		Into(result)
	return
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *imgEncrypters) UpdateStatus(ctx context.Context, imgEncrypter *v1beta1.ImgEncrypter, opts v1.UpdateOptions) (result *v1beta1.ImgEncrypter, err error) {
	result = &v1beta1.ImgEncrypter{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("imgencrypters").
		Name(imgEncrypter.Name).
		SubResource("status").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(imgEncrypter).
		Do(ctx).
		Into(result)
	return
}

// Delete takes name of the imgEncrypter and deletes it. Returns an error if one occurs.
func (c *imgEncrypters) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("imgencrypters").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *imgEncrypters) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	var timeout time.Duration
	if listOpts.TimeoutSeconds != nil {
		timeout = time.Duration(*listOpts.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Namespace(c.ns).
		Resource("imgencrypters").
		VersionedParams(&listOpts, scheme.ParameterCodec).
		Timeout(timeout).
		Body(&opts).
		Do(ctx).
		Error()
}

// Patch applies the patch and returns the patched imgEncrypter.
func (c *imgEncrypters) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1beta1.ImgEncrypter, err error) {
	result = &v1beta1.ImgEncrypter{}
	err = c.client.Patch(pt).
		Namespace(c.ns).
		Resource("imgencrypters").
		Name(name).
		SubResource(subresources...).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}
