package main

import (
	"fmt"
	"log"
	"os"

	"github.com/ktock/buildg/pkg"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run cmd/test_processor/main.go <dockerfile-path>")
	}

	dockerfilePath := os.Args[1]

	// Test the processor
	processor, err := pkg.NewDockerfileProcessor()
	if err != nil {
		log.Fatalf("Failed to create processor: %v", err)
	}

	// Make sure to cleanup
	defer func() {
		if err := processor.Cleanup(); err != nil {
			log.Printf("Cleanup error: %v", err)
		}
	}()

	result, err := processor.Process(dockerfilePath)
	if err != nil {
		log.Fatalf("Failed to process Dockerfile: %v", err)
	}

	fmt.Printf("Processing Results:\n")
	fmt.Printf("Modified Dockerfile: %s\n", result.ModifiedDockerfilePath)
	fmt.Printf("Registry Port: %d\n", result.RegistryPort)
	fmt.Printf("Registry ID: %s\n", result.RegistryID)
	fmt.Printf("Processed Images: %v\n", result.ProcessedImages)
	fmt.Printf("Original Images Found:\n")
	for _, img := range result.OriginalImages {
		fmt.Printf("  Line %d: %s\n", img.Line, img.Original)
	}

	// Read and display the modified Dockerfile if different from original
	if result.ModifiedDockerfilePath != dockerfilePath {
		fmt.Println("\nModified Dockerfile content:")
		content, err := os.ReadFile(result.ModifiedDockerfilePath)
		if err != nil {
			log.Printf("Failed to read modified Dockerfile: %v", err)
		} else {
			fmt.Println(string(content))
		}
	}
}
