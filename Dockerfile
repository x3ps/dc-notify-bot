# Multi-stage Dockerfile for Delta Chat Notify Bot.
# Produces a minimal image (~50 MB) with just the Go binary,
# deltachat-rpc-server, and CA certificates.

# Stage 1: Build the Go binary.
# Uses CGO_ENABLED=0 for a fully static binary that works in any
# Linux image. -trimpath and -ldflags="-s -w" strip debug info and
# file paths to reduce binary size.
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /dc-notify-bot .

# Stage 2: Download the pre-built deltachat-rpc-server binary.
# This avoids compiling Rust from source. TARGETARCH is set
# automatically by Docker BuildKit for multi-platform builds.
FROM debian:bookworm-slim AS fetcher
ARG DELTACHAT_RPC_VERSION=2.45.0
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && ARCH=$(case "${TARGETARCH}" in amd64) echo x86_64;; arm64) echo aarch64;; *) echo "${TARGETARCH}";; esac) \
    && curl -fsSL -o /usr/local/bin/deltachat-rpc-server \
       "https://github.com/chatmail/core/releases/download/v${DELTACHAT_RPC_VERSION}/deltachat-rpc-server-${ARCH}-linux" \
    && chmod +x /usr/local/bin/deltachat-rpc-server

# Stage 3: Minimal runtime image.
# Only CA certificates are installed — needed for TLS connections to
# mail servers. /data is a volume for persistent bot state (Delta Chat
# account database).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /dc-notify-bot /usr/local/bin/
COPY --from=fetcher /usr/local/bin/deltachat-rpc-server /usr/local/bin/
ENV NOTIFY_BOT_LISTEN=0.0.0.0:8080
EXPOSE 8080
VOLUME ["/data"]
WORKDIR /data
CMD ["dc-notify-bot", "serve"]
