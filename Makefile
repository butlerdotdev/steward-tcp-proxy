# Copyright 2025 Butler Labs LLC.
# SPDX-License-Identifier: Apache-2.0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
REGISTRY ?= ghcr.io/butlerdotdev
IMAGE_NAME ?= steward-tcp-proxy
IMAGE_TAG ?= $(VERSION)
IMAGE ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

GOOS ?= linux
GOARCH ?= amd64

.PHONY: all
all: build

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build \
		-ldflags="-s -w -X main.version=$(VERSION)" \
		-o bin/tcp-proxy ./cmd/tcp-proxy

.PHONY: test
test:
	go test -v -race ./...

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker-build
docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE) .

.PHONY: docker-push
docker-push: docker-build
	docker push $(IMAGE)

.PHONY: docker-buildx
docker-buildx:
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE) \
		--push .

.PHONY: clean
clean:
	rm -rf bin/

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build        - Build the binary"
	@echo "  test         - Run tests"
	@echo "  lint         - Run linter"
	@echo "  docker-build - Build Docker image"
	@echo "  docker-push  - Build and push Docker image"
	@echo "  docker-buildx - Multi-arch build and push"
	@echo "  clean        - Remove build artifacts"
