.PHONY: build build-platform-binaries build-mcpb pack-mcpb-from-dist test clean run-dev release-snapshot run-docker run docker-compose-up docker-compose-down lint docker-test

# Variables
BINARY_NAME ?= $(shell basename $(shell git remote get-url origin) .git | sed 's/.*\///')
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_DIR=bin

# Build the application (single binary for local development)
build:
	mkdir -p $(BUILD_DIR)
	go build -ldflags "-X main.Version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd

# Build all platform-specific binaries for MCPB packaging
build-platform-binaries:
	mkdir -p server
	@echo "Building platform-specific binaries for MCPB..."
	GOOS=darwin GOARCH=arm64 go build -ldflags "-X main.Version=$(VERSION)" -o server/$(BINARY_NAME)-darwin-arm64 ./cmd
	GOOS=darwin GOARCH=amd64 go build -ldflags "-X main.Version=$(VERSION)" -o server/$(BINARY_NAME)-darwin-amd64 ./cmd
	GOOS=linux GOARCH=amd64 go build -ldflags "-X main.Version=$(VERSION)" -o server/$(BINARY_NAME)-linux-amd64 ./cmd
	GOOS=windows GOARCH=amd64 go build -ldflags "-X main.Version=$(VERSION)" -o server/$(BINARY_NAME)-windows-amd64.exe ./cmd
	chmod +x server/$(BINARY_NAME)-*
	@echo "All platform binaries built in server/ directory"

# Build MCPB bundle
build-mcpb: build-platform-binaries
	@echo "Creating MCPB bundle..."
	@if [ ! -f "manifest.json" ]; then \
		echo "Error: manifest.json not found"; \
		exit 1; \
	fi
	mkdir -p mcpb-bundle/server
	cp server/* mcpb-bundle/server/
	# Update version and name in manifest.json to match build version and binary name
	sed -e 's/"version": ".*"/"version": "$(VERSION)"/' -e 's/"name": "$(BINARY_NAME)"/"name": "$(BINARY_NAME)"/' manifest.json > mcpb-bundle/manifest.json
	cd mcpb-bundle && zip -r ../$(BINARY_NAME).mcpb . && cd ..
	rm -rf mcpb-bundle
	@echo "MCPB bundle created: $(BINARY_NAME).mcpb"

# Pack MCPB from GoReleaser dist/ folder  
pack-mcpb-from-dist:
	@echo "Creating MCPB bundle from GoReleaser binaries..."
	mkdir -p mcpb-bundle/server
	@# Copy specific platform binaries with explicit names
	@found_count=0; \
	for platform in "linux_amd64" "darwin_amd64" "darwin_arm64" "windows_amd64"; do \
		found=$$(find dist -path "*_$${platform}_*" -name "$(BINARY_NAME)*" -type f | head -1); \
		if [ -n "$$found" ]; then \
			case "$$platform" in \
				"linux_amd64") cp "$$found" "mcpb-bundle/server/$(BINARY_NAME)-linux-amd64" ;; \
				"darwin_amd64") cp "$$found" "mcpb-bundle/server/$(BINARY_NAME)-darwin-amd64" ;; \
				"darwin_arm64") cp "$$found" "mcpb-bundle/server/$(BINARY_NAME)-darwin-arm64" ;; \
				"windows_amd64") cp "$$found" "mcpb-bundle/server/$(BINARY_NAME)-windows-amd64.exe" ;; \
			esac; \
			echo "Copied $$found -> $(BINARY_NAME)-$$platform"; \
			found_count=$$((found_count + 1)); \
		else \
			echo "Warning: No binary found for $$platform"; \
		fi; \
	done; \
	if [ $$found_count -eq 0 ]; then \
		echo "Error: No binaries found in dist/ directory"; \
		exit 1; \
	fi
	@if [ ! -f "manifest.json" ]; then \
		echo "Error: manifest.json not found"; \
		rm -rf mcpb-bundle; \
		exit 1; \
	fi
	# Update version and name in manifest.json to match GoReleaser version and binary name
	@if [ -n "$(GORELEASER_VERSION)" ]; then \
		sed -e 's/"version": ".*"/"version": "$(GORELEASER_VERSION)"/' -e 's/"name": "$(BINARY_NAME)"/"name": "$(BINARY_NAME)"/' manifest.json > mcpb-bundle/manifest.json; \
	else \
		sed -e 's/"version": ".*"/"version": "$(VERSION)"/' -e 's/"name": "$(BINARY_NAME)"/"name": "$(BINARY_NAME)"/' manifest.json > mcpb-bundle/manifest.json; \
	fi
	cd mcpb-bundle && zip -r ../$(BINARY_NAME).mcpb . && cd ..
	rm -rf mcpb-bundle
	@echo "MCPB bundle created: $(BINARY_NAME).mcpb"

# Run tests
test:
	go test ./...

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -rf server
	rm -rf mcpb-bundle
	rm -rf dist
	rm -f $(BINARY_NAME).mcpb

# Run the application in development mode
run-dev:
	go run ./cmd

# Create a release snapshot using GoReleaser
release-snapshot:
	goreleaser release --snapshot --clean
	make pack-mcpb-from-dist

# Run the application using the built binary
run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

# Build and run Docker image
run-docker: build
	docker build -t $(BINARY_NAME):$(VERSION) .
	docker run -p 8080:8080 $(BINARY_NAME):$(VERSION)

# Start the application with Docker Compose
docker-compose-up:
	docker-compose up -d

# Stop Docker Compose services
docker-compose-down:
	docker-compose down

# Run linting checks (same as CI)
lint:
	@echo "Running linters..."
	@go mod tidy
	@if ! git diff --quiet go.mod go.sum; then echo "go.mod or go.sum is not tidy, run 'go mod tidy'"; git diff go.mod go.sum; exit 1; fi
	@if ! command -v golangci-lint &> /dev/null; then echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; fi
	@golangci-lint run --timeout=5m

# Run tests in Docker
docker-test:
	docker build -f Dockerfile.test -t $(BINARY_NAME)-test:$(VERSION) .
	docker run --rm $(BINARY_NAME)-test:$(VERSION)

# Default target
all: clean build
