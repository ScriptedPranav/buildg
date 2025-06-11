package pkg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/sirupsen/logrus"
)

type DockerfileProcessor struct {
	dockerClient *client.Client
	registryPort int
	registryID   string
}

type ImageRef struct {
	Original string
	Modified string
	Line     int
}

type ProcessingResult struct {
	ModifiedDockerfilePath string
	OriginalImages         []ImageRef
	ProcessedImages        []string
	RegistryPort           int
	RegistryID             string
}

func NewDockerfileProcessor() (*DockerfileProcessor, error) {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &DockerfileProcessor{
		dockerClient: dockerClient,
	}, nil
}

func (dp *DockerfileProcessor) Process(dockerfilePath string) (*ProcessingResult, error) {
	// Find a free port for the registry
	port, err := dp.findFreePort()
	if err != nil {
		return nil, fmt.Errorf("failed to find free port: %w", err)
	}
	dp.registryPort = port

	// Start local registry
	registryID, err := dp.startLocalRegistry()
	if err != nil {
		return nil, fmt.Errorf("failed to start local registry: %w", err)
	}
	dp.registryID = registryID

	// Parse Dockerfile and extract images
	images, err := dp.extractImages(dockerfilePath)
	if err != nil {
		dp.Cleanup()
		return nil, fmt.Errorf("failed to extract images: %w", err)
	}

	// Filter images based on criteria
	filteredImages := dp.filterImages(images)
	if len(filteredImages) == 0 {
		logrus.Info("No images to process")
		dp.Cleanup()
		return &ProcessingResult{
			ModifiedDockerfilePath: dockerfilePath,
			OriginalImages:         images,
			ProcessedImages:        []string{},
		}, nil
	}

	// Check if images exist locally
	existingImages, err := dp.checkImagesExist(filteredImages)
	if err != nil {
		dp.Cleanup()
		return nil, err
	}

	// Tag and push images to local registry
	processedImages, err := dp.tagAndPushImages(existingImages)
	if err != nil {
		dp.Cleanup()
		return nil, err
	}

	// Create modified Dockerfile
	modifiedPath, err := dp.createModifiedDockerfile(dockerfilePath, images, processedImages)
	if err != nil {
		dp.Cleanup()
		return nil, err
	}

	return &ProcessingResult{
		ModifiedDockerfilePath: modifiedPath,
		OriginalImages:         images,
		ProcessedImages:        processedImages,
		RegistryPort:           dp.registryPort,
		RegistryID:             dp.registryID,
	}, nil
}

func (dp *DockerfileProcessor) findFreePort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	return addr.Port, nil
}

func (dp *DockerfileProcessor) startLocalRegistry() (string, error) {
	ctx := context.Background()

	// Pull registry image if not exists
	reader, err := dp.dockerClient.ImagePull(ctx, "registry:2", image.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to pull registry image: %w", err)
	}
	io.Copy(io.Discard, reader)
	reader.Close()

	// Create and start registry container
	resp, err := dp.dockerClient.ContainerCreate(ctx, &container.Config{
		Image: "registry:2",
		ExposedPorts: nat.PortSet{
			"5000/tcp": struct{}{},
		},
		Env: []string{
			"REGISTRY_STORAGE_DELETE_ENABLED=true",
		},
	}, &container.HostConfig{
		PortBindings: nat.PortMap{
			"5000/tcp": []nat.PortBinding{{HostPort: strconv.Itoa(dp.registryPort)}},
		},
		AutoRemove: true,
	}, &network.NetworkingConfig{}, nil, "")

	if err != nil {
		return "", fmt.Errorf("failed to create registry container: %w", err)
	}

	if err := dp.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start registry container: %w", err)
	}

	// Wait for registry to be ready
	if err := dp.waitForRegistry(); err != nil {
		dp.dockerClient.ContainerStop(ctx, resp.ID, container.StopOptions{})
		return "", fmt.Errorf("registry failed to start properly: %w", err)
	}

	logrus.Infof("Started local registry on port %d with container ID %s", dp.registryPort, resp.ID)
	return resp.ID, nil
}

func (dp *DockerfileProcessor) waitForRegistry() error {
	maxRetries := 30
	for i := 0; i < maxRetries; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", dp.registryPort), time.Second)
		if err == nil {
			conn.Close()
			time.Sleep(500 * time.Millisecond) // Give it a bit more time to fully start
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("registry did not start within timeout")
}

func (dp *DockerfileProcessor) extractImages(dockerfilePath string) ([]ImageRef, error) {
	file, err := os.Open(dockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open Dockerfile: %w", err)
	}
	defer file.Close()

	var images []ImageRef
	scanner := bufio.NewScanner(file)
	lineNum := 0

	// Map to store ARG values for variable expansion
	argValues := make(map[string]string)

	// Regex patterns for extracting images and ARGs
	fromPattern := regexp.MustCompile(`^\s*FROM\s+([^\s]+)`)
	copyFromPattern := regexp.MustCompile(`^\s*COPY\s+--from=([^\s]+)`)
	argPattern := regexp.MustCompile(`^\s*ARG\s+([A-Z_][A-Z0-9_]*)(?:=(.*))?`)

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// Check for ARG instruction to build variable map
		if matches := argPattern.FindStringSubmatch(line); matches != nil {
			argName := matches[1]
			argValue := ""
			if len(matches) > 2 && matches[2] != "" {
				argValue = matches[2]
			}
			argValues[argName] = argValue
			continue
		}

		// Extract FROM images
		if matches := fromPattern.FindStringSubmatch(line); matches != nil {
			imageName := matches[1]
			// Skip build stage aliases (AS something)
			if asIndex := strings.Index(strings.ToUpper(line), " AS "); asIndex != -1 {
				imageName = strings.TrimSpace(line[strings.Index(strings.ToUpper(line), "FROM")+4 : asIndex])
			}

			// Expand variables in image name
			expandedImageName := dp.expandVariables(imageName, argValues)

			// Skip scratch and build stage references
			if expandedImageName != "scratch" && !dp.isBuildStageReference(expandedImageName) {
				images = append(images, ImageRef{
					Original: expandedImageName,
					Line:     lineNum,
				})
			}
		}

		// Extract COPY --from images
		if matches := copyFromPattern.FindStringSubmatch(line); matches != nil {
			imageName := matches[1]
			// Expand variables in image name
			expandedImageName := dp.expandVariables(imageName, argValues)

			// Skip numeric references (build stages) and scratch
			if !dp.isBuildStageReference(expandedImageName) && expandedImageName != "scratch" {
				images = append(images, ImageRef{
					Original: expandedImageName,
					Line:     lineNum,
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading Dockerfile: %w", err)
	}

	logrus.Infof("Extracted %d images from Dockerfile", len(images))
	return images, nil
}

func (dp *DockerfileProcessor) isBuildStageReference(imageName string) bool {
	// Check if it's a numeric reference (build stage index)
	if _, err := strconv.Atoi(imageName); err == nil {
		return true
	}

	// Check if it's a simple stage name (no registry/tag components)
	// If it doesn't contain ':', '/', or '.', it's likely a stage name
	if !strings.Contains(imageName, ":") && !strings.Contains(imageName, "/") && !strings.Contains(imageName, ".") {
		return true
	}

	return false
}

// expandVariables expands ${VAR} and $VAR patterns in image names using ARG values
func (dp *DockerfileProcessor) expandVariables(text string, argValues map[string]string) string {
	// Handle ${VAR} pattern
	bracePattern := regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)
	text = bracePattern.ReplaceAllStringFunc(text, func(match string) string {
		varName := strings.Trim(strings.Trim(match, "$"), "{}")
		if value, exists := argValues[varName]; exists {
			return value
		}
		// Return original if variable not found (keep unexpanded for error handling)
		return match
	})

	// Handle $VAR pattern (without braces)
	simplePattern := regexp.MustCompile(`\$([A-Z_][A-Z0-9_]*)`)
	text = simplePattern.ReplaceAllStringFunc(text, func(match string) string {
		varName := strings.TrimPrefix(match, "$")
		if value, exists := argValues[varName]; exists {
			return value
		}
		// Return original if variable not found (keep unexpanded for error handling)
		return match
	})

	return text
}

func (dp *DockerfileProcessor) filterImages(images []ImageRef) []ImageRef {
	xdbgImage := os.Getenv("XDBG_IMAGE")
	var filtered []ImageRef

	for _, img := range images {
		if xdbgImage != "" {
			// Exact match for XDBG_IMAGE
			if img.Original == xdbgImage {
				filtered = append(filtered, img)
			}
		} else {
			// Default: substring match for -rfcurated at the end
			if strings.HasSuffix(img.Original, "-rfcurated") {
				filtered = append(filtered, img)
			}
		}
	}

	logrus.Infof("Filtered %d images for processing", len(filtered))
	return filtered
}

func (dp *DockerfileProcessor) checkImagesExist(images []ImageRef) ([]ImageRef, error) {
	ctx := context.Background()
	var existingImages []ImageRef

	for _, img := range images {
		_, _, err := dp.dockerClient.ImageInspectWithRaw(ctx, img.Original)
		if err != nil {
			if client.IsErrNotFound(err) {
				return nil, fmt.Errorf("image %s is not present locally", img.Original)
			}
			return nil, fmt.Errorf("failed to inspect image %s: %w", img.Original, err)
		}
		existingImages = append(existingImages, img)
		logrus.Debugf("Image %s exists locally", img.Original)
	}

	return existingImages, nil
}

func (dp *DockerfileProcessor) tagAndPushImages(images []ImageRef) ([]string, error) {
	ctx := context.Background()
	var processedImages []string

	for _, img := range images {
		localRegistryTag := fmt.Sprintf("localhost:%d/%s", dp.registryPort, img.Original)

		// Tag image for local registry
		if err := dp.dockerClient.ImageTag(ctx, img.Original, localRegistryTag); err != nil {
			return nil, fmt.Errorf("failed to tag image %s: %w", img.Original, err)
		}

		// Push image to local registry
		pushReader, err := dp.dockerClient.ImagePush(ctx, localRegistryTag, image.PushOptions{
			RegistryAuth: "e30K", // Empty JSON object base64 encoded: "{}"
		})
		if err != nil {
			return nil, fmt.Errorf("failed to push image %s: %w", localRegistryTag, err)
		}

		// Read push response to completion
		_, err = io.Copy(io.Discard, pushReader)
		pushReader.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read push response for %s: %w", localRegistryTag, err)
		}

		processedImages = append(processedImages, img.Original)
		logrus.Infof("Successfully tagged and pushed %s as %s", img.Original, localRegistryTag)
	}

	return processedImages, nil
}

func (dp *DockerfileProcessor) createModifiedDockerfile(originalPath string, allImages []ImageRef, processedImages []string) (string, error) {
	// Create map for quick lookup of processed images
	processedMap := make(map[string]bool)
	for _, img := range processedImages {
		processedMap[img] = true
	}

	// Read original file and re-parse to get ARG values and unexpanded image references
	file, err := os.Open(originalPath)
	if err != nil {
		return "", fmt.Errorf("failed to open original Dockerfile: %w", err)
	}
	defer file.Close()

	// Map to store ARG values and line-to-image mapping
	argValues := make(map[string]string)
	lineToUnexpandedImage := make(map[int]string)

	scanner := bufio.NewScanner(file)
	lineNum := 0
	argPattern := regexp.MustCompile(`^\s*ARG\s+([A-Z_][A-Z0-9_]*)(?:=(.*))?`)
	fromPattern := regexp.MustCompile(`^\s*FROM\s+([^\s]+)`)
	copyFromPattern := regexp.MustCompile(`^\s*COPY\s+--from=([^\s]+)`)

	// First pass: collect ARGs and find unexpanded image references
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// Collect ARG values
		if matches := argPattern.FindStringSubmatch(line); matches != nil {
			argName := matches[1]
			argValue := ""
			if len(matches) > 2 && matches[2] != "" {
				argValue = matches[2]
			}
			argValues[argName] = argValue
			continue
		}

		// Find image references and store unexpanded versions
		if matches := fromPattern.FindStringSubmatch(line); matches != nil {
			imageName := matches[1]
			if asIndex := strings.Index(strings.ToUpper(scanner.Text()), " AS "); asIndex != -1 {
				imageName = strings.TrimSpace(scanner.Text()[strings.Index(strings.ToUpper(scanner.Text()), "FROM")+4 : asIndex])
			}
			expandedName := dp.expandVariables(imageName, argValues)
			if processedMap[expandedName] {
				lineToUnexpandedImage[lineNum] = imageName
			}
		}

		if matches := copyFromPattern.FindStringSubmatch(line); matches != nil {
			imageName := matches[1]
			expandedName := dp.expandVariables(imageName, argValues)
			if processedMap[expandedName] && !dp.isBuildStageReference(expandedName) {
				lineToUnexpandedImage[lineNum] = imageName
			}
		}
	}

	// Reset file pointer for second pass
	file.Close()
	input, err := os.Open(originalPath)
	if err != nil {
		return "", fmt.Errorf("failed to reopen original Dockerfile: %w", err)
	}
	defer input.Close()

	// Create modified file path
	dir := filepath.Dir(originalPath)
	baseName := filepath.Base(originalPath)
	modifiedPath := filepath.Join(dir, baseName+".buildg-local")

	output, err := os.Create(modifiedPath)
	if err != nil {
		return "", fmt.Errorf("failed to create modified Dockerfile: %w", err)
	}
	defer output.Close()

	// Second pass: write modified file
	scanner = bufio.NewScanner(input)
	lineNum = 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		modifiedLine := line

		// Check if this line contains an image we need to replace
		if unexpandedImage, exists := lineToUnexpandedImage[lineNum]; exists {
			expandedImage := dp.expandVariables(unexpandedImage, argValues)
			localRegistryImage := fmt.Sprintf("localhost:%d/%s", dp.registryPort, expandedImage)
			modifiedLine = strings.ReplaceAll(line, unexpandedImage, localRegistryImage)
			logrus.Debugf("Line %d: replaced %s with %s", lineNum, unexpandedImage, localRegistryImage)
		}

		if _, err := fmt.Fprintln(output, modifiedLine); err != nil {
			return "", fmt.Errorf("failed to write to modified Dockerfile: %w", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading original Dockerfile: %w", err)
	}

	logrus.Infof("Created modified Dockerfile: %s", modifiedPath)
	return modifiedPath, nil
}

func (dp *DockerfileProcessor) Cleanup() error {
	if dp.dockerClient == nil {
		return nil
	}

	ctx := context.Background()
	var errors []string

	// Stop and remove registry container
	if dp.registryID != "" {
		if err := dp.dockerClient.ContainerStop(ctx, dp.registryID, container.StopOptions{}); err != nil {
			errors = append(errors, fmt.Sprintf("failed to stop registry container: %v", err))
		}
		// Container will be auto-removed due to AutoRemove: true
		logrus.Infof("Stopped and removed registry container %s", dp.registryID)
	}

	// Remove any tagged images for local registry
	images, err := dp.dockerClient.ImageList(ctx, image.ListOptions{})
	if err == nil && dp.registryPort != 0 {
		registryPrefix := fmt.Sprintf("localhost:%d/", dp.registryPort)
		for _, img := range images {
			for _, tag := range img.RepoTags {
				if strings.HasPrefix(tag, registryPrefix) {
					if _, err := dp.dockerClient.ImageRemove(ctx, tag, image.RemoveOptions{}); err != nil {
						logrus.Debugf("Failed to remove tagged image %s: %v", tag, err)
					} else {
						logrus.Debugf("Removed tagged image %s", tag)
					}
				}
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errors, ", "))
	}

	return nil
}

// Helper function to run buildg with the preprocessed Dockerfile
func RunBuildgWithProcessedDockerfile(result *ProcessingResult, originalArgs []string) error {
	if result == nil {
		return fmt.Errorf("processing result is nil")
	}

	// Create new args with modified Dockerfile
	newArgs := make([]string, len(originalArgs))
	copy(newArgs, originalArgs)

	// Find and replace the -f or --file flag
	for i, arg := range newArgs {
		if arg == "-f" || arg == "--file" {
			if i+1 < len(newArgs) {
				newArgs[i+1] = result.ModifiedDockerfilePath
				logrus.Infof("Replaced Dockerfile path with: %s", result.ModifiedDockerfilePath)
			}
		} else if strings.HasPrefix(arg, "-f=") || strings.HasPrefix(arg, "--file=") {
			parts := strings.SplitN(arg, "=", 2)
			if len(parts) == 2 {
				newArgs[i] = parts[0] + "=" + result.ModifiedDockerfilePath
				logrus.Infof("Replaced Dockerfile path with: %s", result.ModifiedDockerfilePath)
			}
		}
	}

	// Execute the original buildg logic with modified args
	logrus.Infof("Running buildg with modified arguments")
	return runOriginalBuildg(newArgs)
}

// Placeholder for the original buildg logic
func runOriginalBuildg(args []string) error {
	// This would call the original debugAction or main buildg logic
	// For now, we'll just log the args
	logrus.Infof("Would run buildg with args: %v", args)

	// In the actual implementation, this would be replaced with:
	// return debugAction(clicontext)
	// where clicontext is constructed from the modified args

	return nil
}
