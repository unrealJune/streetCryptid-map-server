# syntax=docker/dockerfile:1

# --- build stage -------------------------------------------------------------
# Pin the Go minor version. Bump deliberately, not with :latest.
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Copy modules first so dependency resolution is cached independently of source.
COPY go.mod go.sum* ./
RUN go mod download

# Now the source.
COPY . .

# Run the full test suite (including the race detector) before we build the
# image, so a broken commit cannot be published. CI gates tests natively on
# amd64 and sets RUN_TESTS=false for the emulated multi-arch build to avoid
# running the race detector under qemu.
ARG RUN_TESTS=true
RUN if [ "$RUN_TESTS" = "true" ]; then CGO_ENABLED=1 go test -race ./...; fi

# Build metadata injected into the binary.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Static, trimmed, non-cgo binary. -ldflags injects version/commit/date and
# strips debug info for a smaller image.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X main.version=${VERSION} \
      -X main.commit=${COMMIT} \
      -X main.date=${BUILD_DATE}" \
    -o /out/streetcryptid-map-server \
    ./cmd/server

# --- final stage -------------------------------------------------------------
# Distroless static: no shell, no package manager, no libc. Runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot

# OCI metadata.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="streetcryptid-map-server" \
      org.opencontainers.image.description="Privacy-quantized OpenMapTiles map API (coarse XYZ + z10 SCB1 bundles) with self-bootstrapping tile data" \
      org.opencontainers.image.source="https://github.com/junephilip/streetcryptid-map-server" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=build /out/streetcryptid-map-server /usr/local/bin/streetcryptid-map-server

# Distroless nonroot is uid 65532.
USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/streetcryptid-map-server"]
CMD ["serve"]
