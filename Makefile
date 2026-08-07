BINARY  ?= local-argocd-renderer
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST    ?= dist
IMAGE   ?= ghcr.io/pkalemba/local-argocd-renderer

# Platforms the release binaries are built for.
PLATFORMS ?= linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# Platforms the container image is built for.
IMAGE_PLATFORMS ?= linux/amd64,linux/arm64

GO      ?= go
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all
all: test build

.PHONY: build
build:
	$(GOBUILD) -o $(BINARY) ./cmd/local-argocd-renderer

.PHONY: test
test:
	$(GO) test -short ./...

.PHONY: e2e
e2e:
	$(GO) test ./e2e/...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

# Cross-compile the binaries for every platform in PLATFORMS into $(DIST).
.PHONY: dist
dist: clean-dist
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		out="$(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch}$$ext"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o "$$out" ./cmd/local-argocd-renderer || exit 1; \
	done

.PHONY: checksums
checksums: dist
	cd $(DIST) && sha256sum * > checksums.txt

# Build the multi-platform image. Loading a multi-platform build into the local
# daemon is not supported, so this only builds it; use image-push to publish.
.PHONY: image
image:
	docker buildx build --platform $(IMAGE_PLATFORMS) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) .

.PHONY: image-push
image-push:
	docker buildx build --platform $(IMAGE_PLATFORMS) --build-arg VERSION=$(VERSION) \
		-t $(IMAGE):$(VERSION) -t $(IMAGE):latest --push .

# Build an image for the current platform and load it into the local daemon.
.PHONY: image-local
image-local:
	docker buildx build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) --load .

.PHONY: clean-dist
clean-dist:
	rm -rf $(DIST)

.PHONY: clean
clean: clean-dist
	rm -f $(BINARY)
