# =============================================================================
# Builder: compile the fordjent binary
# =============================================================================
FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

# Cache-busting argument — pass --build-arg CACHE_BUST=$(date +%s) to force rebuild
ARG CACHE_BUST=0
RUN echo "Cache bust: ${CACHE_BUST}"

COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o fordjent ./cmd/fordjent

# =============================================================================
# Slim target: fordjent binary only (~80 MB)
# No Go toolchain — verify gates (go build, go test) won't work.
# Use this for deployments where verify gates run on external runners
# or where only event processing is needed.
# =============================================================================
FROM debian:bookworm-slim AS slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git bubblewrap && rm -rf /var/lib/apt/lists/*

RUN bwrap --version

RUN useradd -m -d /var/lib/fordjent -s /bin/sh fordjent \
    && mkdir -p /var/lib/fordjent/work \
    && chown -R fordjent:fordjent /var/lib/fordjent

COPY --from=builder /build/fordjent /usr/local/bin/fordjent
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /var/lib/fordjent
VOLUME ["/var/lib/fordjent"]

USER fordjent
EXPOSE 8080

ENTRYPOINT ["entrypoint.sh"]

# =============================================================================
# Full target: includes Go toolchain + Python scientific stack (~450 MB)
# Uses Debian bookworm for glibc compatibility with sklearn/scipy wheels.
# Supports go build/test, Python ML packages, and verify gates.
# This is the default target.
# =============================================================================
FROM golang:1.25-bookworm AS full

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential git ca-certificates curl bubblewrap \
    python3 python3-pip python3-venv python3-dev && rm -rf /var/lib/apt/lists/*

RUN bwrap --version

# golangci-lint — only in the full image where verify gates run
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /usr/local/bin v1.64.8

# Pre-install scientific Python stack (sklearn, scipy have pre-built wheels on Debian)
RUN pip3 install --break-system-packages numpy scipy scikit-learn matplotlib pytest

RUN useradd -m -d /var/lib/fordjent -s /bin/sh fordjent \
    && mkdir -p /var/lib/fordjent/work /var/cache/go-build /var/cache/go-mod \
    && chown -R fordjent:fordjent /var/lib/fordjent /var/cache/go-build /var/cache/go-mod

COPY --from=builder /build/fordjent /usr/local/bin/fordjent
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /var/lib/fordjent
VOLUME ["/var/lib/fordjent"]

USER fordjent
EXPOSE 8080

ENTRYPOINT ["entrypoint.sh"]