FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o fordjent ./cmd/fordjent

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl git && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b /usr/local/bin v1.64.8

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
