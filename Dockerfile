# =============================================================================
# Builder: compile the fordjent binary
# =============================================================================
FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
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
# Full target: includes Go toolchain for verify gates (~320 MB)
# Uses Alpine + build-base instead of Debian + build-essential.
# Supports go build, go test, go vet, and CGO (for sqlite etc).
# This is the default target.
# =============================================================================
FROM golang:1.25-alpine AS full

RUN apk add --no-cache build-base git ca-certificates curl bubblewrap

RUN bwrap --version

# golangci-lint — only in the full image where verify gates run
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /usr/local/bin v1.64.8

RUN adduser -D -h /var/lib/fordjent -s /bin/sh fordjent \
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