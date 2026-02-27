package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	crdaemon "github.com/google/go-containerregistry/pkg/v1/daemon"
	crtypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// ImportImage streams an image that exists in the local Docker daemon into the
// provided content store. Layers are deduplicated by digest; existing content
// is skipped. On success, the returned descriptor points to the manifest of the
// imported image.
func ImportImage(ctx context.Context, store content.Store, lm leases.Manager, ref string) (ocispec.Descriptor, error) {
	if strings.TrimSpace(ref) == "" {
		return ocispec.Descriptor{}, fmt.Errorf("image reference must be specified")
	}
	// Ensure content is pinned by a lease so GC won't drop it.
	lease, err := lm.Create(ctx, leases.WithRandomID())
	if err == nil {
		ctx = leases.WithLease(ctx, lease.ID)
	}

	// Resolve image from Docker daemon.
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	img, err := crdaemon.Image(parsed)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to resolve image from docker daemon: %w", err)
	}

	// Write config blob
	rcfg, err := img.RawConfigFile()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	cfgHash, err := img.ConfigName()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	cfgDesc := ocispec.Descriptor{
		MediaType: string(crtypes.OCIConfigJSON),
		Digest:    digest.Digest(cfgHash.String()),
		Size:      int64(len(rcfg)),
	}
	if err := writeBytesIfMissing(ctx, store, cfgDesc, rcfg); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("config ingest failed: %w", err)
	}

	// Write layers (compressed)
	layers, err := img.Layers()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	// Preallocate to avoid re-allocations while appending
	layerDescs := make([]ocispec.Descriptor, 0, len(layers))
	// Reusable buffer for streaming compressed layers to reduce syscall overhead
	copyBuf := make([]byte, 4<<20) // 4 MiB
	for i, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		sz, err := l.Size()
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		mt, err := l.MediaType()
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		desc := ocispec.Descriptor{MediaType: string(mt), Digest: digest.Digest(d.String()), Size: sz}
		if err := writeLayerIfMissing(ctx, store, desc, l, copyBuf, i, len(layers)); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("layer %d ingest failed: %w", i, err)
		}
		layerDescs = append(layerDescs, desc)
	}

	// Write manifest
	rm, err := img.RawManifest()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	md, err := img.Digest()
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: string(crtypes.OCIManifestSchema1),
		Digest:    digest.Digest(md.String()),
		Size:      int64(len(rm)),
	}
	if err := writeBytesIfMissing(ctx, store, manifestDesc, rm); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("manifest ingest failed: %w", err)
	}

	return manifestDesc, nil
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

func writeLayerIfMissing(ctx context.Context, store content.Store, desc ocispec.Descriptor, l v1.Layer, buf []byte, idx int, total int) error {
	if _, err := store.Info(ctx, desc.Digest); err == nil {
		logrus.Debugf("skip layer %d/%d %s (already present)", idx+1, total, shortDigest(desc.Digest.Encoded()))
		return nil
	}
	start := time.Now()
	logrus.Debugf("importing layer %d/%d %s size=%.2fMB", idx+1, total, shortDigest(desc.Digest.Encoded()), bytesToMiB(desc.Size))
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
	if _, err := io.CopyBuffer(w, rc, buf); err != nil {
		return err
	}
	if err := w.Commit(ctx, desc.Size, desc.Digest); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return err
		}
	}
	elapsed := time.Since(start)
	throughput := float64(0)
	if desc.Size > 0 && elapsed > 0 {
		throughput = bytesToMiB(desc.Size) / elapsed.Seconds()
	}
	logrus.Debugf("imported  layer %d/%d %s in %s (%.2f MB/s)", idx+1, total, shortDigest(desc.Digest.Encoded()), elapsed.Truncate(10*time.Millisecond), throughput)
	return nil
}

func shortDigest(encoded string) string {
	// encoded is usually like "sha256:abcdef..."
	const shortLen = 12
	if i := strings.IndexByte(encoded, ':'); i >= 0 && i+1 < len(encoded) {
		hex := encoded[i+1:]
		if len(hex) > shortLen {
			return hex[:shortLen]
		}
		return hex
	}
	if len(encoded) > shortLen {
		return encoded[:shortLen]
	}
	return encoded
}

func bytesToMiB(n int64) float64 {
	return float64(n) / (1024.0 * 1024.0)
}
