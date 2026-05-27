# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.22-bookworm AS build
ARG ONNXRUNTIME_VERSION=1.16.0
WORKDIR /src

# Fetch the ONNX Runtime native library (aarch64) to bundle into the runtime
# image. The binding (yalue/onnxruntime_go) dlopens this at runtime.
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
# CGO_ENABLED=0: the ONNX binding loads libonnxruntime via purego/dlopen, so no
# C toolchain is needed. CONTINGENCY: if this ever fails with an "import "C"" or
# linker error (a future binding version reintroducing cgo), set
# CGO_ENABLED=1 and add `gcc` here:
#   RUN apt-get update && apt-get install -y --no-install-recommends gcc && rm -rf /var/lib/apt/lists/*
#   RUN CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/server ./cmd/server

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
