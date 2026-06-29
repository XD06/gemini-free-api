# syntax=docker/dockerfile:1

# Stage 1: Build
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Optional: speed up module downloads for users behind the GFW.
# Override with: docker compose build --build-arg GOPROXY=off
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

# Cache dependencies — layer is only invalidated when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Build binary — BuildKit cache mounts preserve the Go build cache across
# rebuilds, so incremental compilation kicks in even after COPY . . changes.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o main ./cmd/server/main.go

# Stage 2: Final Image
FROM alpine:3.22.2

# Install required packages. wget is used by the compose healthcheck; su-exec
# drops privileges after fixing bind-mounted runtime directory ownership.
RUN apk add --no-cache ca-certificates tzdata wget su-exec

# Create a non-root user and group
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /home/appuser

# Copy file from builder and change ownership
COPY --from=builder --chown=appuser:appgroup /app/main .
COPY --chown=root:root docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh

# Pre-create runtime data dirs so named-volume mounts inherit appuser ownership.
# App writes to: .cookies/, data/cookies/, data/state/ (see configs.go defaults).
RUN mkdir -p .cookies data/cookies data/state && chown -R appuser:appgroup /home/appuser

EXPOSE 8787

ENTRYPOINT ["docker-entrypoint.sh"]
