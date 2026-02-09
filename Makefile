BINARY    := mikrotik-kube
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
ARCH      ?= arm64
GOFLAGS   := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build build-local image tarball test lint clean

## Build the Go binary for the target architecture
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build $(GOFLAGS) -o dist/$(BINARY)-$(ARCH) ./cmd/mikrotik-kube/

## Build for the host platform (development)
build-local:
	go build $(GOFLAGS) -o dist/$(BINARY) ./cmd/mikrotik-kube/

## Build the container image
image:
	docker buildx build --platform linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(BINARY):$(VERSION)-$(ARCH) --load .

## Export as RouterOS-compatible rootfs tarball
tarball: image
	@mkdir -p dist
	@bash hack/build.sh $(ARCH)

## Push tarball to a MikroTik device
push: tarball
	PUSH_TO_DEVICE=$(DEVICE) bash hack/build.sh $(ARCH)

## Run tests
test:
	go test -v -race ./...

## Lint
lint:
	golangci-lint run ./...

## Clean build artifacts
clean:
	rm -rf dist/

## Generate mocks for testing
mocks:
	mockgen -source=pkg/routeros/client.go -destination=pkg/routeros/mock_client.go -package=routeros
