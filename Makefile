ALPINE_VERSION := 3.19.1
ASSETS_DIR := internal/vm/assets
BIN_DIR := bin
ISO_URL := https://dl-cdn.alpinelinux.org/alpine/v3.19/releases/x86_64/alpine-virt-$(ALPINE_VERSION)-x86_64.iso
MINIROOTFS_URL := https://dl-cdn.alpinelinux.org/alpine/v3.19/releases/x86_64/alpine-minirootfs-$(ALPINE_VERSION)-x86_64.tar.gz

.PHONY: all build build-cli build-agent clean assets kernel rootfs initramfs sign test test-unit test-integration test-coverage

# Default target: build CLI for macOS
all: build-cli sign

# Build CLI (main binary)
build-cli: assets
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/awsocks ./cmd/awsocks

# Legacy build target (for Phase 0 compatibility)
build: assets
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/awsocks ./cmd/awsocks

# Build agent for Linux (used inside VM)
build-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-s -w' -o tmp/agent ./cmd/agent

# Sign the binary (required for Virtualization.framework)
sign: $(BIN_DIR)/awsocks
	codesign --entitlements entitlements.plist -s - $(BIN_DIR)/awsocks

# Unit tests only (fast, no external dependencies)
test-unit:
	go test -short ./...

# Integration tests (requires macOS, VM assets)
test-integration:
	go test -tags=integration ./...

# All tests
test: test-unit

# Coverage report
test-coverage:
	go test -short -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Phase 1 assets (Go agent based initramfs) - default
assets: $(ASSETS_DIR)/vmlinuz-virt $(ASSETS_DIR)/initramfs.cpio.gz

# Phase 0 assets (Alpine Linux based) - legacy
assets-phase0: $(ASSETS_DIR)/vmlinuz-virt $(ASSETS_DIR)/initramfs-virt $(ASSETS_DIR)/rootfs.img.gz

# Download kernel and initramfs from Alpine ISO
$(ASSETS_DIR)/vmlinuz-virt $(ASSETS_DIR)/initramfs-virt:
	@echo "==> Downloading Alpine ISO and extracting kernel..."
	@mkdir -p $(ASSETS_DIR) tmp
	@curl -L -o tmp/alpine.iso $(ISO_URL)
	@cd tmp && bsdtar -xf alpine.iso boot/vmlinuz-virt boot/initramfs-virt
	@mv tmp/boot/vmlinuz-virt tmp/boot/initramfs-virt $(ASSETS_DIR)/
	@rm -rf tmp
	@echo "==> Kernel and initramfs ready"

# Build rootfs using Docker (Phase 0)
$(ASSETS_DIR)/rootfs.img.gz:
	@echo "==> Building rootfs using Docker..."
	@./scripts/build-rootfs.sh
	@echo "==> rootfs ready"

# Build initramfs with Go agent (Phase 1)
$(ASSETS_DIR)/initramfs.cpio.gz: build-agent
	@echo "==> Building initramfs with Go agent..."
	@./scripts/build-initramfs.sh
	@echo "==> initramfs ready"

clean:
	rm -rf $(BIN_DIR)
	rm -rf tmp

clean-assets:
	rm -f $(ASSETS_DIR)/vmlinuz-virt $(ASSETS_DIR)/initramfs-virt $(ASSETS_DIR)/rootfs.img.gz $(ASSETS_DIR)/initramfs.cpio.gz

clean-all: clean clean-assets

# Run direct mode (no VM) for testing
run-direct:
	go run ./cmd/awsocks start --mode direct

# Run VM mode
run-vm: build-cli sign
	./$(BIN_DIR)/awsocks start --mode vm
