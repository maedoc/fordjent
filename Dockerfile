FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o fordjent ./cmd/fordjent

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl git golang-go && rm -rf /var/lib/apt/lists/*

RUN useradd -m -d /var/lib/fordjent -s /bin/sh fordjent \
    && mkdir -p /var/lib/fordjent/work \
    && chown -R fordjent:fordjent /var/lib/fordjent

# Configure git identity globally so the agent can commit
RUN git config --global user.email "fordjent@forgejo.local" \
    && git config --global user.name "Fordjent Agent" \
    && git config --global push.default current \
    && cp /root/.gitconfig /var/lib/fordjent/.gitconfig \
    && chown fordjent:fordjent /var/lib/fordjent/.gitconfig

COPY --from=builder /build/fordjent /usr/local/bin/fordjent

WORKDIR /var/lib/fordjent
VOLUME ["/var/lib/fordjent"]

USER fordjent

EXPOSE 8080

ENTRYPOINT ["fordjent", "-config", "/etc/fordjent/fordjent.yaml"]
