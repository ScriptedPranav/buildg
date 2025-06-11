package buildkit

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

// DockerFallbackManager handles Docker daemon fallback for local images
type DockerFallbackManager struct {
	dockerHost   string
	client       *dockerclient.Client
	contentStore content.Store
}

// NewDockerFallbackManager creates a new Docker fallback manager
func NewDockerFallbackManager(dockerHost string, contentStore content.Store) (*DockerFallbackManager, error) {
	var client *dockerclient.Client
	var err error

	if dockerHost != "" {
		client, err = dockerclient.NewClientWithOpts(
			dockerclient.WithHost(dockerHost),
			dockerclient.WithAPIVersionNegotiation(),
		)
	} else {
		client, err = dockerclient.NewClientWithOpts(
			dockerclient.FromEnv,
			dockerclient.WithAPIVersionNegotiation(),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &DockerFallbackManager{
		dockerHost:   dockerHost,
		client:       client,
		contentStore: contentStore,
	}, nil
}

// CheckImageExists checks if an image exists in Docker daemon
func (d *DockerFallbackManager) CheckImageExists(ctx context.Context, imageRef string) (bool, error) {
	if d.client == nil {
		return false, fmt.Errorf("Docker client not initialized")
	}

	// List images and check for the reference
	images, err := d.client.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list Docker images: %w", err)
	}

	// Normalize the image reference for comparison
	normalizedRef := normalizeImageRef(imageRef)

	for _, img := range images {
		for _, tag := range img.RepoTags {
			if normalizeImageRef(tag) == normalizedRef {
				logrus.Infof("Found image %s in Docker daemon with tag %s", imageRef, tag)
				return true, nil
			}
		}
		// Also check RepoDigests
		for _, digest := range img.RepoDigests {
			if normalizeImageRef(digest) == normalizedRef {
				logrus.Infof("Found image %s in Docker daemon with digest %s", imageRef, digest)
				return true, nil
			}
		}
	}

	return false, nil
}

// ListImages lists all images in the Docker daemon
func (d *DockerFallbackManager) ListImages(ctx context.Context) ([]image.Summary, error) {
	images, err := d.client.ImageList(ctx, image.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list images from Docker daemon: %w", err)
	}
	return images, nil
}

// normalizeImageRef normalizes image references for comparison
func normalizeImageRef(ref string) string {
	// Handle docker.io library normalization
	if strings.HasPrefix(ref, "docker.io/library/") {
		ref = strings.TrimPrefix(ref, "docker.io/library/")
	} else if strings.HasPrefix(ref, "library/") {
		ref = strings.TrimPrefix(ref, "library/")
	}

	// Add latest tag if no tag specified
	if !strings.Contains(ref, ":") && !strings.Contains(ref, "@") {
		ref = ref + ":latest"
	}

	return ref
}

// ImportImageFromDocker imports an image from Docker daemon to BuildKit
func (d *DockerFallbackManager) ImportImageFromDocker(ctx context.Context, imageRef string) error {
	if d.client == nil {
		return fmt.Errorf("Docker client not initialized")
	}

	logrus.Infof("Starting Docker daemon image import for %s", imageRef)

	// Export image from Docker daemon as a tar stream
	reader, err := d.client.ImageSave(ctx, []string{imageRef})
	if err != nil {
		return fmt.Errorf("failed to export image %s from Docker daemon: %w", imageRef, err)
	}
	defer reader.Close()

	// TODO: Import the tar stream into BuildKit's content store
	// This is a complex process that involves:
	// 1. Parsing the Docker image tar format
	// 2. Converting Docker image format to OCI format
	// 3. Importing layers and manifests into BuildKit's content store
	// 4. Making the image available for BuildKit's registry resolver

	logrus.Infof("Successfully exported image %s from Docker daemon (import to BuildKit pending)", imageRef)
	return nil
}

// CreateBuildKitConfig creates a BuildKit configuration with Docker fallback
func (d *DockerFallbackManager) CreateBuildKitConfig() (string, error) {
	// Create a buildkitd.toml configuration that includes registry mirrors
	// pointing to a local registry proxy that can serve Docker daemon images
	config := `
debug = true

# Registry configuration for Docker daemon fallback
[registry."docker.io"]
  # Use mirrors to redirect failed registry requests to our Docker daemon fallback
  mirrors = ["docker-daemon-fallback.local"]

[registry."docker-daemon-fallback.local"]
  # This is a placeholder for the Docker daemon fallback mechanism
  # In a full implementation, this would point to a local registry proxy
  # that serves images from the Docker daemon
  insecure = true
`
	return config, nil
}

// Close closes the Docker client connection
func (d *DockerFallbackManager) Close() error {
	if d.client != nil {
		return d.client.Close()
	}
	return nil
}

// SetupDockerFallbackMirror sets up the registry mirror configuration for Docker fallback
func SetupDockerFallbackMirror(configPath string, dockerHost string) error {
	// This function would write a buildkitd.toml configuration file
	// that sets up registry mirrors to handle Docker daemon fallback

	manager, err := NewDockerFallbackManager(dockerHost, nil)
	if err != nil {
		return fmt.Errorf("failed to create Docker fallback manager: %w", err)
	}
	defer manager.Close()

	config, err := manager.CreateBuildKitConfig()
	if err != nil {
		return fmt.Errorf("failed to create BuildKit config: %w", err)
	}

	logrus.Infof("Docker fallback configuration created for host: %s", dockerHost)
	logrus.Debugf("BuildKit config: %s", config)

	// TODO: Write config to file
	// This would write the configuration to configPath

	return nil
}
