package buildkit

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	ctrimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	importerdaemon "github.com/ktock/buildg/pkg/importer/daemon"
	"github.com/moby/buildkit/cmd/buildkitd/config"
	"github.com/sirupsen/logrus"
)

// autoImportImageStore wraps a ctrimages.Store and, on a Get miss, attempts to
// import the image from the local Docker daemon into the shared content store.
// On successful import it registers the mapping and returns it transparently.
type autoImportImageStore struct {
	inner ctrimages.Store
	cfg   *config.Config
	cs    content.Store
	lm    leases.Manager
}

func newAutoImportImageStore(inner ctrimages.Store, cfg *config.Config) ctrimages.Store {
	return &autoImportImageStore{inner: inner, cfg: cfg}
}

// setStores injects the content store and lease manager from the active worker.
// Until these are set, auto-import is disabled to avoid recursive initialization.
func (s *autoImportImageStore) setStores(cs content.Store, lm leases.Manager) {
	s.cs = cs
	s.lm = lm
}

func (s *autoImportImageStore) Get(ctx context.Context, name string) (ctrimages.Image, error) {
	// First, try the underlying store.
	img, err := s.inner.Get(ctx, name)
	if err == nil {
		return img, nil
	}
	if !errdefs.IsNotFound(err) {
		return img, err
	}

	// If we don't yet have stores wired, skip auto-import to avoid re-entrant init.
	if s.cs == nil || s.lm == nil {
		return img, err
	}

	// Not found: attempt to import from Docker daemon into our content store.
	logrus.Debugf("auto-import: %q not found locally; attempting import from Docker daemon", name)
	manifestDesc, iErr := importerdaemon.ImportImage(ctx, s.cs, s.lm, name, 1)
	if iErr != nil {
		logrus.WithError(iErr).Debugf("auto-import: failed to import %q from Docker daemon", name)
		return img, err
	}
	// Register mapping in the local image store.
	if _, cErr := s.inner.Create(ctx, ctrimages.Image{Name: name, Target: manifestDesc}); cErr != nil {
		if _, uErr := s.inner.Update(ctx, ctrimages.Image{Name: name, Target: manifestDesc}); uErr != nil {
			// Ignore inability to update; fall back to final Get
			logrus.WithError(uErr).Debugf("auto-import: failed to update mapping for %q", name)
		}
	}
	// Retry Get and return the result (or the original error if still missing).
	return s.inner.Get(ctx, name)
}

func (s *autoImportImageStore) List(ctx context.Context, filters ...string) ([]ctrimages.Image, error) {
	return s.inner.List(ctx, filters...)
}

func (s *autoImportImageStore) Create(ctx context.Context, image ctrimages.Image) (ctrimages.Image, error) {
	return s.inner.Create(ctx, image)
}

func (s *autoImportImageStore) Update(ctx context.Context, image ctrimages.Image, fieldpaths ...string) (ctrimages.Image, error) {
	return s.inner.Update(ctx, image, fieldpaths...)
}

func (s *autoImportImageStore) Delete(ctx context.Context, name string, opts ...ctrimages.DeleteOpt) error {
	return s.inner.Delete(ctx, name, opts...)
}
