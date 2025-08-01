.PHONY: help build clean test lint fmt vet install deps run-check run-disable

# Default target
help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Build the binary
build: ## Build the binary
	go build -o ec2-checker .

# Build for multiple platforms
build-all: ## Build for multiple platforms
	GOOS=linux GOARCH=amd64 go build -o bin/ec2-checker-linux-amd64 .
	GOOS=darwin GOARCH=amd64 go build -o bin/ec2-checker-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 go build -o bin/ec2-checker-darwin-arm64 .
	GOOS=windows GOARCH=amd64 go build -o bin/ec2-checker-windows-amd64.exe .

# Clean build artifacts
clean: ## Clean build artifacts
	rm -f ec2-checker
	rm -rf bin/
	rm -f *.csv
	rm -f *.log

# Run tests
test: ## Run tests
	go test -v ./...

# Run tests with coverage
test-coverage: ## Run tests with coverage
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Lint the code
lint: ## Run golangci-lint
	golangci-lint run

# Format the code
fmt: ## Format Go code
	go fmt ./...

# Vet the code
vet: ## Run go vet
	go vet ./...

# Install dependencies
deps: ## Install dependencies
	go mod tidy
	go mod download

# Update dependencies
deps-update: ## Update dependencies
	go get -u ./...
	go mod tidy

# Install the binary to GOPATH/bin
install: build ## Install binary to GOPATH/bin
	cp ec2-checker $(GOPATH)/bin/

# Run check command (example)
run-check: build ## Run check command with example profiles
	@echo "Usage: make run-check PROFILES='profile1 profile2'"
	@if [ -z "$(PROFILES)" ]; then \
		echo "Please set PROFILES variable: make run-check PROFILES='profile1 profile2'"; \
		exit 1; \
	fi
	./ec2-checker check $(PROFILES)

# Run disable command (example)
run-disable: build ## Run disable command with example profiles
	@echo "Usage: make run-disable PROFILES='profile1 profile2'"
	@if [ -z "$(PROFILES)" ]; then \
		echo "Please set PROFILES variable: make run-disable PROFILES='profile1 profile2'"; \
		exit 1; \
	fi
	./ec2-checker disable $(PROFILES)

# Run with dry-run
run-dry-run: build ## Run disable with dry-run
	@echo "Usage: make run-dry-run PROFILES='profile1 profile2'"
	@if [ -z "$(PROFILES)" ]; then \
		echo "Please set PROFILES variable: make run-dry-run PROFILES='profile1 profile2'"; \
		exit 1; \
	fi
	./ec2-checker disable -dry-run $(PROFILES)

# Development setup
dev-setup: ## Setup development environment
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go mod tidy

# Create example config
config-example: ## Create example config file
	cp config.example.yaml config.yaml
	@echo "Created config.yaml from config.example.yaml"
	@echo "Please edit config.yaml to customize settings"


# Release preparation
release-prep: clean fmt vet test build-all ## Prepare for release
	@echo "Release preparation complete"
	@echo "Built binaries are in bin/ directory"

# Check for security vulnerabilities
security-check: ## Check for security vulnerabilities
	go list -json -deps ./... | nancy sleuth

# Show Go version and environment
info: ## Show Go version and environment info
	@echo "Go version:"
	@go version
	@echo ""
	@echo "Go environment:"
	@go env GOOS GOARCH
	@echo ""
	@echo "Module info:"
	@go list -m all | head -1