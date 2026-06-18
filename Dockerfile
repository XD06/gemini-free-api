# Stage 1: Build
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o main ./cmd/server/main.go

# Stage 2: Final Image
FROM alpine:3.22.2

# Install required packages. wget is used by the compose healthcheck.
RUN apk add --no-cache ca-certificates tzdata wget

# Create a non-root user and group
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /home/appuser

# Copy file from builder and change ownership
COPY --from=builder --chown=appuser:appgroup /app/main .

# Pre-create runtime data dirs so named-volume mounts inherit appuser ownership.
# App writes to: .cookies/, data/cookies/, data/state/ (see configs.go defaults).
RUN mkdir -p .cookies data/cookies data/state && chown -R appuser:appgroup /home/appuser

# Switch to non-root user
USER appuser

EXPOSE 8787

ENTRYPOINT ["./main"]
