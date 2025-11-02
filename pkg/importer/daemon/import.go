package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	crdaemon "github.com/google/go-containerregistry/pkg/v1/daemon"
	crtypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImportImage streams an image that exists in the local Docker daemon into the
// provided content store. Layers are deduplicated by digest; existing content
// is skipped. On success, the returned digest is the manifest digest of the
// imported image.
func ImportImage(ctx context.Context, store content.Store, lm leases.Manager, ref string) (digest.Digest, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("image reference must be specified")
	}
	// Ensure content is pinned by a lease so GC won't drop it.
	lease, err := lm.Create(ctx, leases.WithRandomID())
	if err == nil {
		ctx = leases.WithLease(ctx, lease.ID)
	}

	// Resolve image from Docker daemon.
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	img, err := crdaemon.Image(parsed)
	if err != nil {
		return "", fmt.Errorf("failed to resolve image from docker daemon: %w", err)
	}

	// Write config blob
	rcfg, err := img.RawConfigFile()
	if err != nil {
		return "", err
	}
	cfgHash, err := img.ConfigName()
	if err != nil {
		return "", err
	}
	cfgDesc := ocispec.Descriptor{
		MediaType: string(crtypes.OCIConfigJSON),
		Digest:    digest.Digest(cfgHash.String()),
		Size:      int64(len(rcfg)),
	}
	if err := writeBytesIfMissing(ctx, store, cfgDesc, rcfg); err != nil {
		return "", fmt.Errorf("config ingest failed: %w", err)
	}

	// Write layers (compressed)
	layers, err := img.Layers()
	if err != nil {
		return "", err
	}
	var layerDescs []ocispec.Descriptor
	for i, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return "", err
		}
		sz, err := l.Size()
		if err != nil {
			return "", err
		}
		mt, err := l.MediaType()
		if err != nil {
			return "", err
		}
		desc := ocispec.Descriptor{MediaType: string(mt), Digest: digest.Digest(d.String()), Size: sz}
		if err := writeLayerIfMissing(ctx, store, desc, l); err != nil {
			return "", fmt.Errorf("layer %d ingest failed: %w", i, err)
		}
		layerDescs = append(layerDescs, desc)
	}

	// Write manifest
	rm, err := img.RawManifest()
	if err != nil {
		return "", err
	}
	md, err := img.Digest()
	if err != nil {
		return "", err
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: string(crtypes.OCIManifestSchema1),
		Digest:    digest.Digest(md.String()),
		Size:      int64(len(rm)),
	}
	if err := writeBytesIfMissing(ctx, store, manifestDesc, rm); err != nil {
		return "", fmt.Errorf("manifest ingest failed: %w", err)
	}

	return manifestDesc.Digest, nil
}

func writeBytesIfMissing(ctx context.Context, store content.Store, desc ocispec.Descriptor, b []byte) error {
	if _, err := store.Info(ctx, desc.Digest); err == nil {
		return nil
	}
	w, err := store.Writer(ctx, content.WithRef("import-"+desc.Digest.Encoded()))
	if err != nil {
		return err
	}
	defer w.Close()
	if _, err := io.Copy(w, bytes.NewReader(b)); err != nil {
		return err
	}
	if err := w.Commit(ctx, desc.Size, desc.Digest); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

func writeLayerIfMissing(ctx context.Context, store content.Store, desc ocispec.Descriptor, l v1.Layer) error {
	if _, err := store.Info(ctx, desc.Digest); err == nil {
		return nil
	}
	rc, err := l.Compressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := store.Writer(ctx, content.WithRef("import-"+desc.Digest.Encoded()))
	if err != nil {
		return err
	}
	defer w.Close()
	if _, err := io.Copy(w, rc); err != nil {
		return err
	}
	if err := w.Commit(ctx, desc.Size, desc.Digest); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}
