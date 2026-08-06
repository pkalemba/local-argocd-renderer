# syntax=docker/dockerfile:1

# Build with buildx to produce a multi-platform image:
#   docker buildx build --platform linux/amd64,linux/arm64 -t local-argocd-renderer .

ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.21
ARG HELM_VERSION=v3.16.4
ARG KUSTOMIZE_VERSION=v5.5.0

# Cross-compile the renderer from the build platform for the target platform.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/local-argocd-renderer ./cmd/local-argocd-renderer

# Fetch the target platform's helm and kustomize binaries, which the renderer shells
# out to. This runs on the build platform, so no emulation is involved.
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS tools

ARG TARGETOS
ARG TARGETARCH
ARG HELM_VERSION
ARG KUSTOMIZE_VERSION

RUN apk add --no-cache ca-certificates curl tar

RUN curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz" \
        | tar -xz -C /tmp \
    && install -D -m 0755 "/tmp/${TARGETOS}-${TARGETARCH}/helm" /out/helm

RUN curl -fsSL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/kustomize_${KUSTOMIZE_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz" \
        | tar -xz -C /tmp \
    && install -D -m 0755 /tmp/kustomize /out/kustomize

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates git

COPY --from=tools /out/helm /usr/local/bin/helm
COPY --from=tools /out/kustomize /usr/local/bin/kustomize
COPY --from=build /out/local-argocd-renderer /usr/local/bin/local-argocd-renderer

# Remote Helm charts are cached below XDG_CACHE_HOME, which has to be writable for
# the non-root user.
ENV XDG_CACHE_HOME=/tmp/cache
ENV HELM_CACHE_HOME=/tmp/cache/helm
ENV HELM_CONFIG_HOME=/tmp/config/helm
ENV HELM_DATA_HOME=/tmp/data/helm

# The repository to render is expected to be mounted here:
#   docker run --rm -v "$PWD:/repo" local-argocd-renderer --app app.yaml
WORKDIR /repo
USER nobody

ENTRYPOINT ["local-argocd-renderer"]
