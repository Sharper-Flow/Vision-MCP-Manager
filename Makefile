# Vision - Go-native MCP Server Daemon
# Makefile for build, test, lint, and release tasks

BINARY := vision
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)"

GO := go
GOFLAGS := -v
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

# Directories
CMD_DIR := ./cmd/vision
BIN_DIR := ./bin
DIST_DIR := ./dist

.PHONY: all build test lint clean fmt vet install dev run help

# Default target
all: lint test build

# Build the binary
build:
	@echo "Building $(BINARY)..."
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BIN_DIR)/$(BINARY) $(CMD_DIR)

# Build for current platform (alias)
dev: build

# Run without building
run:
	$(GO) run $(CMD_DIR) $(ARGS)

# Install to GOPATH/bin
install:
	$(GO) install $(LDFLAGS) $(CMD_DIR)

# Run all tests
test:
	$(GO) test -race -cover ./...

# Run tests with coverage report
test-coverage:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run linter (requires golangci-lint)
lint:
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run ./...

# Format code
fmt:
	$(GO) fmt ./...
	@which goimports > /dev/null && goimports -w . || true

# Vet code
vet:
	$(GO) vet ./...

# Clean build artifacts
clean:
	@rm -rf $(BIN_DIR) $(DIST_DIR)
	@rm -f coverage.out coverage.html
	@echo "Cleaned build artifacts"

# Tidy dependencies
tidy:
	$(GO) mod tidy

# Update dependencies
update:
	$(GO) get -u ./...
	$(GO) mod tidy

# Cross-compile for multiple platforms
dist:
	@mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY)-linux-amd64 $(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY)-linux-arm64 $(CMD_DIR)
	GOOS=darwin GOARCH=amd64 $(GO) build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY)-darwin-amd64 $(CMD_DIR)
	GOOS=darwin GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(DIST_DIR)/$(BINARY)-darwin-arm64 $(CMD_DIR)
	@echo "Built binaries in $(DIST_DIR)"

# Show version info
version:
	@echo "Version: $(VERSION)"
	@echo "Commit:  $(COMMIT)"
	@echo "Built:   $(BUILD_TIME)"

# Help
help:
	@echo "Vision - Go-native MCP Server Daemon"
	@echo ""
	@echo "Usage:"
	@echo "  make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all           Run lint, test, and build (default)"
	@echo "  build         Build the binary"
	@echo "  dev           Alias for build"
	@echo "  run           Run without building (use ARGS=... for arguments)"
	@echo "  install       Install to GOPATH/bin"
	@echo "  test          Run all tests with race detection"
	@echo "  test-coverage Run tests with coverage report"
	@echo "  lint          Run golangci-lint"
	@echo "  fmt           Format code"
	@echo "  vet           Run go vet"
	@echo "  clean         Remove build artifacts"
	@echo "  tidy          Tidy go.mod"
	@echo "  update        Update dependencies"
	@echo "  dist          Cross-compile for all platforms"
	@echo "  version       Show version info"
	@echo "  help          Show this help"
