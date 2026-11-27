# Copyright 2026 Ksolves India Limited.
# SPDX-License-Identifier: Apache-2.0
# Build the manager binary
FROM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Ksolves Operator for Apache NiFi" \
      org.opencontainers.image.description="Controller manager of the Ksolves Operator for Apache NiFi" \
      org.opencontainers.image.vendor="Ksolves India Limited" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/ksolves/nifi-operator" \
      org.opencontainers.image.documentation="https://github.com/ksolves/nifi-operator/tree/main/docs" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
WORKDIR /
COPY --from=builder /workspace/manager .
# Apache-2.0 section 4: the licence and the bundled modules' licences and NOTICEs.
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.txt /licenses/
USER 65532:65532

ENTRYPOINT ["/manager"]
