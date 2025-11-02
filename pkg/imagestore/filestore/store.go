package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Store implements containerd images.Store backed by a simple JSON file.
// It is read/write safe and only stores name -> target descriptor mapping.
// Labels and timestamps are minimally populated.
type Store struct {
	mu   sync.RWMutex
	path string
	// name -> descriptor
	m map[string]ocispec.Descriptor
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return nil, err
	}
	s := &Store{path: root, m: map[string]ocispec.Descriptor{}}
	s.load()
	return s, nil
}

func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var mm map[string]ocispec.Descriptor
	if json.Unmarshal(b, &mm) == nil {
		s.m = mm
	}
}

func (s *Store) persist() error {
	tmp := s.path + ".tmp"
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Get(ctx context.Context, name string) (images.Image, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.m[name]
	if !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	now := time.Now()
	return images.Image{
		Name:      name,
		Target:    d,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (s *Store) List(ctx context.Context, filters ...string) ([]images.Image, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []images.Image
	for name, d := range s.m {
		out = append(out, images.Image{Name: name, Target: d, CreatedAt: now, UpdatedAt: now})
	}
	return out, nil
}

func (s *Store) Create(ctx context.Context, image images.Image) (images.Image, error) {
	if image.Name == "" {
		return images.Image{}, errors.New("image name must be set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[image.Name]; ok {
		return images.Image{}, errdefs.ErrAlreadyExists
	}
	s.m[image.Name] = image.Target
	if err := s.persist(); err != nil {
		return images.Image{}, err
	}
	now := time.Now()
	image.CreatedAt = now
	image.UpdatedAt = now
	return image, nil
}

func (s *Store) Update(ctx context.Context, image images.Image, fieldpaths ...string) (images.Image, error) {
	if image.Name == "" {
		return images.Image{}, errors.New("image name must be set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[image.Name]; !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	s.m[image.Name] = image.Target
	if err := s.persist(); err != nil {
		return images.Image{}, err
	}
	image.UpdatedAt = time.Now()
	return image, nil
}

func (s *Store) Delete(ctx context.Context, name string, opts ...images.DeleteOpt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[name]; !ok {
		return errdefs.ErrNotFound
	}
	delete(s.m, name)
	return s.persist()
}
