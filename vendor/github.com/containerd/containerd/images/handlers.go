package images

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd/content"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var SkipDesc = fmt.Errorf("skip descriptor")

var StopHandler = fmt.Errorf("stop handler")

type Handler interface {
	Handle(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error)
}

type HandlerFunc func(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error)

func (fn HandlerFunc) Handle(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error) {
	return fn(ctx, desc)
}

// Handlers returns a handler that will run the handlers in sequence.
//
// A handler may return `StopHandler` to stop calling additional handlers
func Handlers(handlers ...Handler) HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) (subdescs []ocispec.Descriptor, err error) {
		var children []ocispec.Descriptor
		for _, handler := range handlers {
			ch, err := handler.Handle(ctx, desc)
			if err != nil {
				if errors.Cause(err) == StopHandler {
					break
				}
				return nil, err
			}

			children = append(children, ch...)
		}

		return children, nil
	}
}

// Walk the resources of an image and call the handler for each. If the handler
// decodes the sub-resources for each image,
//
// This differs from dispatch in that each sibling resource is considered
// synchronously.
func Walk(ctx context.Context, handler Handler, descs ...ocispec.Descriptor) error {
	for _, desc := range descs {

		children, err := handler.Handle(ctx, desc)
		if err != nil {
			if errors.Cause(err) == SkipDesc {
				return nil // don't traverse the children.
			}
			return err
		}

		if len(children) > 0 {
			if err := Walk(ctx, handler, children...); err != nil {
				return err
			}
		}
	}

	return nil
}

// Dispatch runs the provided handler for content specified by the descriptors.
// If the handler decode subresources, they will be visited, as well.
//
// Handlers for siblings are run in parallel on the provided descriptors. A
// handler may return `SkipDesc` to signal to the dispatcher to not traverse
// any children.
//
// Typically, this function will be used with `FetchHandler`, often composed
// with other handlers.
//
// If any handler returns an error, the dispatch session will be canceled.
func Dispatch(ctx context.Context, handler Handler, descs ...ocispec.Descriptor) error {
	eg, ctx := errgroup.WithContext(ctx)
	for _, desc := range descs {
		desc := desc

		eg.Go(func() error {
			desc := desc

			children, err := handler.Handle(ctx, desc)
			if err != nil {
				if errors.Cause(err) == SkipDesc {
					return nil // don't traverse the children.
				}
				return err
			}

			if len(children) > 0 {
				return Dispatch(ctx, handler, children...)
			}

			return nil
		})
	}

	return eg.Wait()
}

// ChildrenHandler decodes well-known manifests types and returns their children.
//
// This is useful for supporting recursive fetch and other use cases where you
// want to do a full walk of resources.
//
// One can also replace this with another implementation to allow descending of
// arbitrary types.
func ChildrenHandler(provider content.Provider) HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		case MediaTypeDockerSchema2Layer, MediaTypeDockerSchema2LayerGzip,
			MediaTypeDockerSchema2Config:
			return nil, nil
		default:
			return nil, fmt.Errorf("%v not yet supported", desc.MediaType)
		}

		p, err := content.ReadBlob(ctx, provider, desc.Digest)
		if err != nil {
			return nil, err
		}

		// TODO(stevvooe): We just assume oci manifest, for now. There may be
		// subtle differences from the docker version.
		var manifest ocispec.Manifest
		if err := json.Unmarshal(p, &manifest); err != nil {
			return nil, err
		}

		var descs []ocispec.Descriptor

		descs = append(descs, manifest.Config)
		descs = append(descs, manifest.Layers...)

		return descs, nil
	}
}
