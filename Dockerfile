# ---- build stage ----
FROM golang:1.22-bookworm AS build

WORKDIR /src
COPY backend/go.mod backend/go.sum* ./
# go.sum doesn't exist until first `go mod download` — fine to be optional
RUN go mod download || true

COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server

# ---- runtime stage ----
FROM debian:bookworm-slim

# ca-certificates lets us hit HF for asset download if needed; ffmpeg lets
# users post-process WAV→MP4/MP3 from the manifest if they want.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/server /app/server
COPY frontend /app/frontend

# These three are bind-mounted via docker-compose so they survive container
# restarts and are visible on the host filesystem. The empty placeholder dirs
# in the image just give the bind mounts something to land on.
RUN mkdir -p /app/assets /app/inbox /app/outbox

ENV SUPERTONIC_ASSETS=/app/assets \
    SUPERTONIC_INBOX=/app/inbox \
    SUPERTONIC_OUTBOX=/app/outbox \
    SUPERTONIC_FRONTEND=/app/frontend \
    SUPERTONIC_PORT=8080

EXPOSE 8080
ENTRYPOINT ["/app/server"]
