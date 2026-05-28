# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.25-bookworm AS build
ARG ONNXRUNTIME_VERSION=1.18.0
WORKDIR /src

# Fetch the ONNX Runtime native library (aarch64) to bundle into the runtime
# image. The binding (yalue/onnxruntime_go v1.11.0) dlopens this at runtime and
# requests ORT C API version 18, so the bundled library must be >= 1.18.0; the
# binding is built against 1.18.0 headers exactly, so we pin that.
RUN set -eux; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-aarch64-${ONNXRUNTIME_VERSION}.tgz"; \
    mkdir -p /opt/onnxruntime; \
    tar -xzf /tmp/ort.tgz -C /opt/onnxruntime --strip-components=1; \
    rm /tmp/ort.tgz

# Dependency layer first for cache hits.
COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./
# yalue/onnxruntime_go uses cgo (import "C") for its dlopen plumbing, so the
# build needs a C compiler and CGO_ENABLED=1. The native libonnxruntime.so is
# only dlopen'd at runtime (via ONNXRUNTIME_LIB_PATH), not linked at build time,
# and the debian runtime stage provides the glibc this binary links against.
RUN apt-get update && apt-get install -y --no-install-recommends gcc \
    && rm -rf /var/lib/apt/lists/*
RUN CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o /out/server ./cmd/server

# ---- runtime stage ----
FROM debian:bookworm-slim

# ca-certificates for HTTPS; ffmpeg is for users' optional post-processing
# (the pipeline itself does not call it).
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# Bundle the ONNX Runtime shared library and register it with the linker.
COPY --from=build /opt/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/
RUN ldconfig

WORKDIR /app
COPY --from=build /out/server /app/server
COPY frontend /app/frontend

# Placeholder dirs for the bind mounts to land on.
RUN mkdir -p /app/assets /app/inbox /app/outbox

ENV SUPERTONIC_ASSETS=/app/assets \
    SUPERTONIC_INBOX=/app/inbox \
    SUPERTONIC_OUTBOX=/app/outbox \
    SUPERTONIC_FRONTEND=/app/frontend \
    SUPERTONIC_PORT=8080 \
    ONNXRUNTIME_LIB_PATH=/usr/local/lib/libonnxruntime.so

EXPOSE 8080
ENTRYPOINT ["/app/server"]
