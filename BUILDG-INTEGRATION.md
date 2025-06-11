# BuildG Local Image Preprocessing Integration

This implementation adds automatic preprocessing of Dockerfiles to handle local images that buildg cannot access through its standalone buildkit instance.

## Problem Solved

BuildG uses its own standalone buildkit instance and cannot access Docker images that are stored locally but not in registries. This integration solves this by:

1. **Automatic Detection**: Parsing Dockerfiles to find image references in `FROM` and `COPY --from` instructions
2. **Smart Filtering**: Selecting only relevant images (those ending with `-rfcurated` by default, or exact matches via `XDBG_IMAGE` env var)
3. **Local Registry**: Creating a temporary local Docker registry 
4. **Image Transfer**: Tagging and pushing local images to the temporary registry
5. **Dockerfile Modification**: Creating a modified Dockerfile with localhost registry references
6. **Cleanup**: Automatically cleaning up registry, images, and temporary files

## Features

### Image Selection Criteria

- **Default**: Images ending with `-rfcurated` suffix (substring match at end)
- **Override**: Set `XDBG_IMAGE=exact-image-name` for exact match processing

### Robust Dockerfile Parsing

- Handles `FROM image AS stage` syntax
- Handles `COPY --from=image` syntax  
- Skips `scratch` and build stage references
- Preserves line numbers for accurate replacement

### Error Handling

- Fails fast if required local images are missing
- Proper cleanup on errors
- Detailed logging throughout the process

### File Management

- Creates modified Dockerfile with `.buildg-local` suffix
- Preserves original Dockerfile unchanged
- Automatic cleanup of temporary files

## Usage

The preprocessing is automatic and transparent when using buildg:

```bash
# Standard usage - will preprocess automatically if applicable images are found
buildg debug -f Dockerfile.test .

# Override to process specific image only
XDBG_IMAGE="my-custom-image:tag" buildg debug -f Dockerfile .

# No special flags needed - preprocessing happens seamlessly
```

## Example Output

```
INFO[0002] Preprocessing Dockerfile for local images...
INFO[0002] Started local registry on port 43337 
INFO[0002] Extracted 2 images from Dockerfile           
INFO[0002] Filtered 2 images for processing             
INFO[0051] Successfully tagged and pushed golang:1.24.4-jammy-rfcurated as localhost:43337/golang:1.24.4-jammy-rfcurated
INFO[0060] Successfully tagged and pushed rfubu:22.04-rfapt-rfcurated as localhost:43337/rfubu:22.04-rfapt-rfcurated 
INFO[0060] Created modified Dockerfile: Dockerfile.buildg-local 
INFO[0060] Processed 2 local images, using modified Dockerfile: Dockerfile.buildg-local
```

## Technical Implementation

### Core Components

1. **DockerfileProcessor** (`pkg/dockerfile_processor.go`): Main processing logic
2. **Integration** (`main.go`): Seamless integration into existing buildg flow
3. **Docker Client**: Uses official Docker API for registry and image operations

### Processing Flow

1. **Parse Arguments**: Extract Dockerfile path from CLI args
2. **Image Extraction**: Parse Dockerfile with regex patterns for image references
3. **Filtering**: Apply selection criteria (rfcurated suffix or XDBG_IMAGE exact match)  
4. **Registry Setup**: Start temporary local Docker registry on random port
5. **Image Processing**: Tag and push selected local images to temporary registry
6. **Dockerfile Modification**: Create new Dockerfile with localhost registry references
7. **BuildG Execution**: Run original buildg logic with modified Dockerfile
8. **Cleanup**: Stop registry, remove tagged images, delete temporary files

### Key Design Decisions

- **Non-caching**: Always processes fresh for simplicity and reliability
- **Fail-fast**: Exits immediately if required local images are missing  
- **Flexible**: Code structured to easily accommodate future caching if needed
- **Clean**: Preserves CLI flag compatibility and transparent operation

## Testing

The implementation includes comprehensive testing:

```bash
# Test with real local images
go run cmd/test_processor/main.go Dockerfile.real

# Test filtering logic
XDBG_IMAGE="specific-image" go run cmd/test_processor/main.go Dockerfile.test
```

## Future Enhancements

- **Caching**: Cache registry state between runs for same Dockerfile
- **Variable Expansion**: Handle ARG/ENV variables in image names
- **Advanced Filtering**: Regex patterns or multiple image selection
- **Performance**: Parallel image processing for large Dockerfiles 