# Copyright 2025 Butler Labs LLC.
# SPDX-License-Identifier: Apache-2.0

# Build stage
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o tcp-proxy ./cmd/tcp-proxy

# Runtime stage - distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/tcp-proxy .

USER 65532:65532

ENTRYPOINT ["/tcp-proxy"]
