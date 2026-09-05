# Makefile for Git Server S3

.PHONY: help test test-unit test-integration test-coverage build run clean lint fmt vet deps

# Variables
BINARY_NAME=git-server-s3
BUILD_DIR=tmp
CONFIG_FILE=config.yaml

# Help
help: ## Display this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# Install dependencies
deps: ## Install dependencies
	go mod download
	go mod tidy

# Unit tests
test-unit: ## Run unit tests
	go test -v -race -timeout 5m ./...

# Tests with coverage
test-coverage: ## Run tests with coverage
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./pkg/... ./internal/...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Integration tests against a running Floci container.
test-integration: ## Run Floci integration tests (requires FLOCI_ENDPOINT)
	@test -n "$(FLOCI_ENDPOINT)" || (echo "Set FLOCI_ENDPOINT, e.g. http://127.0.0.1:4566"; exit 1)
	go test -race -tags=integration -count=1 -timeout=5m ./... -run Floci

# All tests
test: test-unit ## Run all tests (unit tests only by default)

# Tests with more verbosity
test-verbose: ## Run tests with more details
	go test -v -race -coverprofile=coverage.out ./pkg/... ./internal/... -args -test.v

# Code formatting
fmt: ## Format code
	go fmt ./...

# Static checks
vet: ## Check code with go vet
	go vet ./...

# Linting (requires golangci-lint)
lint: ## Lint code (requires golangci-lint)
	golangci-lint run

# Build
build: ## Build the server
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd

# Build with version information
build-release: ## Build for release
	@mkdir -p $(BUILD_DIR)
	go build -ldflags "-X main.version=$(shell git describe --tags --always --dirty)" \
		-o $(BUILD_DIR)/$(BINARY_NAME) ./cmd

# Run the server
run: build ## Build and run the server
	./$(BUILD_DIR)/$(BINARY_NAME) server --config $(CONFIG_FILE)

# Run in development mode with auto-reload (requires air)
dev: ## Run in development mode (requires air)
	@if command -v air >/dev/null 2>&1; then \
		air; \
	else \
		echo "⚠️  air not installed. Installation:"; \
		echo "go install github.com/cosmtrek/air@latest"; \
		echo "Or use: make run"; \
	fi

# Cleanup
clean: ## Clean generated files
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html
	go clean -cache -testcache

# Performance tests
bench: ## Run benchmarks
	go test -bench=. -benchmem -run=^$$ ./pkg/... ./internal/...

# Race condition tests
race: ## Run tests with race condition detection
	go test -race ./pkg/... ./internal/...

# Security check (requires gosec)
security: ## Check code security (requires gosec)
	@if command -v gosec >/dev/null 2>&1; then \
		gosec ./...; \
	else \
		echo "⚠️  gosec not installed. Installation:"; \
		echo "go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest"; \
	fi

# Update dependencies
update-deps: ## Update dependencies
	go get -u ./...
	go mod tidy

# Complete check (CI)
ci: lint vet test-unit ## CI/CD checks

# Memory tests with Valgrind (Linux only)
memcheck: ## Memory tests (Linux only)
	@if command -v valgrind >/dev/null 2>&1; then \
		go test -c ./pkg/storage/s3/ && valgrind --leak-check=full ./s3.test; \
	else \
		echo "⚠️  valgrind not available (Linux only)"; \
	fi

# Install development tools
install-tools: ## Install development tools
	@echo "Install golangci-lint v2.13.2 from the official release linked in README.md"
	go install github.com/cosmtrek/air@latest
	go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest

# Test configuration example
setup-test-env: ## Display test configuration example
	@echo "Configuration for integration tests:"
	@echo ""
	@echo "# Local MinIO (for testing)"
	@echo "export S3_TEST_BUCKET=git-server-test"
	@echo "export S3_TEST_REGION=us-east-1"
	@echo "export S3_TEST_ENDPOINT=http://localhost:9000"
	@echo "export AWS_ACCESS_KEY_ID=minio"
	@echo "export AWS_SECRET_ACCESS_KEY=minio123"
	@echo ""
	@echo "# Real AWS S3 (for cloud testing)"
	@echo "export S3_TEST_BUCKET=your-test-bucket"
	@echo "export S3_TEST_REGION=us-east-1"
	@echo "export S3_TEST_ENDPOINT=https://s3.us-east-1.amazonaws.com"
	@echo "# + your AWS credentials via AWS CLI or environment variables"

# Tests with different log levels
test-debug: ## Run tests with debug logs
	ZEROLOG_LEVEL=debug go test -v ./pkg/... ./internal/...

# Tests on specific files
test-s3: ## Test only the S3 package
	go test -v ./pkg/storage/s3/...

test-api: ## Test only the API
	go test -v ./internal/api/...

# Test statistics
test-stats: ## Test statistics
	@echo "📊 Test statistics:"
	@find . -name "*_test.go" -not -path "./vendor/*" | wc -l | xargs echo "Test files:"
	@grep -r "func Test" --include="*_test.go" . | wc -l | xargs echo "Test functions:"
	@grep -r "func Benchmark" --include="*_test.go" . | wc -l | xargs echo "Benchmarks:"
