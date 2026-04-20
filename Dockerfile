FROM golang:latest AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o fordjent ./cmd/fordjent

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/fordjent /usr/local/bin/fordjent

WORKDIR /var/lib/fordjent
VOLUME ["/var/lib/fordjent"]

EXPOSE 8080

ENTRYPOINT ["fordjent", "-config", "/etc/fordjent/fordjent.yaml"]
