# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build

# gcc and musl-dev are required: mattn/go-sqlite3 is a cgo package, so this
# cannot be built with CGO_ENABLED=0.
RUN apk add --no-cache gcc musl-dev

WORKDIR /src

# Dependencies first, so editing application code does not re-download them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Statically linked, so the runtime stage needs no matching musl and the two
# Alpine versions can drift apart freely. -trimpath keeps build paths out of
# the binary.
#
# The two cache mounts are what keep rebuilds short. They live on the build
# host rather than in a layer, so they survive a deploy that invalidates every
# layer above: /go/pkg/mod skips re-downloading modules, and /root/.cache/go-build
# skips recompiling anything unchanged. That second one matters most here,
# because go-sqlite3 compiles SQLite's ~250k-line C amalgamation and that is the
# single slowest step of a cold build.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux go build \
    -trimpath \
    -ldflags='-s -w -extldflags "-static"' \
    -o /out/aislide .

FROM alpine:3.21

# The app makes outbound HTTPS calls to RevenueCat and to Google's key endpoint
# for Firebase token verification. Alpine ships no trust store of its own, so
# without this every one of those calls fails with an x509 error.
RUN apk add --no-cache ca-certificates

# Run as a non-root user, and give it ownership of the data directory. Docker
# copies this ownership into a fresh named volume mounted at /data, so the
# container can write there without running as root. A bind mount from the host
# does not inherit it: see the note below.
RUN addgroup -g 10001 -S app \
    && adduser -u 10001 -S -G app app \
    && mkdir -p /data \
    && chown app:app /data

COPY --from=build /out/aislide /usr/local/bin/aislide

# The database lives in a directory, not just a file: SQLite runs in WAL mode
# here, which keeps aislide.db-wal and aislide.db-shm alongside it. Mounting the
# file alone would break the moment SQLite tried to create those, so /data is
# the mount point and the whole directory must be writable.
ENV DATABASE_PATH=/data/aislide.db
VOLUME /data

# release mode drops gin's debug output, including per-route registration logs.
ENV GIN_MODE=release
ENV PORT=8000
EXPOSE 8000

USER app

# Shell form on purpose: it expands PORT at runtime, so overriding the port does
# not silently leave the healthcheck probing the old one. wget is busybox's,
# already in the base image.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO /dev/null "http://127.0.0.1:${PORT}/healthz" || exit 1

CMD ["aislide"]
