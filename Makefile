# P2P File Sync Engine - Makefile
# Cross-platform build system for the P2P sync engine

# Build variables
BINARY_NAME := p2psync
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)"

# Go settings
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod
GOFMT := $(GOCMD) fmt
GOVET := $(GOCMD) vet

# Directories
CMD_DIR := ./cmd/p2psync
BUILD_DIR := ./build
COVERAGE_DIR := ./coverage

# OS detection for Windows compatibility
ifeq ($(OS),Windows_NT)
    BINARY := $(BUILD_DIR)/$(BINARY_NAME).exe
    RM := del /Q /F
    RMDIR := rmdir /S /Q
    MKDIR := mkdir
    SEP := \\
else
    BINARY := $(BUILD_DIR)/$(BINARY_NAME)
    RM := rm -f
    RMDIR := rm -rf
    MKDIR := mkdir -p
    SEP := /
endif

.PHONY: all build clean test test-verbose test-coverage lint fmt vet deps tidy run install help

# Default target
all: deps build

## Build targets

build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@$(MKDIR) $(BUILD_DIR) 2>/dev/null || true
	$(GOBUILD) $(LDFLAGS) -o $(BINARY) $(CMD_DIR)
	@echo "Built: $(BINARY)"

build-all: ## Build for all platforms
	@echo "Building for multiple platforms..."
	@$(MKDIR) $(BUILD_DIR) 2>/dev/null || true
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(CMD_DIR)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(CMD_DIR)
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(CMD_DIR)
	@echo "Built binaries in $(BUILD_DIR)/"

install: build ## Install the binary to GOPATH/bin
	@echo "Installing $(BINARY_NAME)..."
	$(GOCMD) install $(LDFLAGS) $(CMD_DIR)
	@echo "Installed to $(shell go env GOPATH)/bin/$(BINARY_NAME)"

## Test targets

test: ## Run all tests
	@echo "Running tests..."
	$(GOTEST) ./...

test-verbose: ## Run tests with verbose output
	@echo "Running tests (verbose)..."
	$(GOTEST) -v ./...

test-race: ## Run tests with race detector
	@echo "Running tests with race detector..."
	$(GOTEST) -race ./...

test-coverage: ## Run tests with coverage report
	@echo "Running tests with coverage..."
	@$(MKDIR) $(COVERAGE_DIR) 2>/dev/null || true
	$(GOTEST) -coverprofile=$(COVERAGE_DIR)/coverage.out ./...
	$(GOCMD) tool cover -html=$(COVERAGE_DIR)/coverage.out -o $(COVERAGE_DIR)/coverage.html
	@echo "Coverage report: $(COVERAGE_DIR)/coverage.html"

bench: ## Run benchmarks
	@echo "Running benchmarks..."
	$(GOTEST) -bench=. -benchmem ./...

## Code quality targets

lint: ## Run linter (requires golangci-lint)
	@echo "Running linter..."
	golangci-lint run ./...

fmt: ## Format code
	@echo "Formatting code..."
	$(GOFMT) ./...

vet: ## Run go vet
	@echo "Running go vet..."
	$(GOVET) ./...

check: fmt vet ## Run all code quality checks
	@echo "All checks passed!"

## Dependency management

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	$(GOMOD) download

tidy: ## Tidy dependencies
	@echo "Tidying dependencies..."
	$(GOMOD) tidy

verify: ## Verify dependencies
	@echo "Verifying dependencies..."
	$(GOMOD) verify

## Run targets

run: build ## Build and run the binary
	@echo "Running $(BINARY_NAME)..."
	$(BINARY) --help

run-serve: build ## Run the sync daemon
	$(BINARY) serve --metrics :9090

run-watch: build ## Run file watcher
	$(BINARY) watch

## Clean targets

clean: ## Remove build artifacts
	@echo "Cleaning..."
	@$(RMDIR) $(BUILD_DIR) 2>/dev/null || true
	@$(RMDIR) $(COVERAGE_DIR) 2>/dev/null || true
	@echo "Cleaned!"

clean-all: clean ## Remove all generated files including test cache
	@echo "Cleaning test cache..."
	$(GOCMD) clean -testcache
	@echo "Done!"

## Protobuf generation

proto: ## Generate protobuf code
	@echo "Generating protobuf code..."
	protoc --go_out=. --go_opt=paths=source_relative internal/proto/sync.proto
	@echo "Generated internal/proto/sync.pb.go"

## Docker targets (optional)

docker-build: ## Build Docker image
	@echo "Building Docker image..."
	docker build -t $(BINARY_NAME):$(VERSION) .

docker-run: ## Run Docker container
	docker run --rm -it $(BINARY_NAME):$(VERSION)

## Development helpers

dev-setup: deps ## Setup development environment
	@echo "Setting up development environment..."
	$(GOCMD) install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Development environment ready!"

watch-test: ## Watch for changes and run tests (requires watchexec)
	watchexec -e go -- $(GOTEST) ./...

## Documentation

docs: ## Generate documentation
	@echo "Generating documentation..."
	$(GOCMD) doc -all ./...

## Version info

version: ## Show version info
	@echo "Version: $(VERSION)"
	@echo "Commit:  $(COMMIT)"
	@echo "Date:    $(BUILD_DATE)"

## Help

help: ## Show this help
	@echo "P2P File Sync Engine - Build System"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
