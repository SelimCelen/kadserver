# Build stage
FROM golang:1.24.1-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    git \
    make \
    gcc \
    musl-dev

WORKDIR /app

# Download go modules first (better layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags='-w -s -extldflags "-static"' -o /krelay .

# Final stage
FROM alpine:3.18

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    && addgroup -S krelay \
    && adduser -S krelay -G krelay

WORKDIR /app

# Copy artifacts from builder
COPY --from=builder --chown=krelay:krelay /krelay /app/krelay


# Create data directory
RUN mkdir -p /data && chown krelay:krelay /data



# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:${API_PORT:-5000}/api/v1/info || exit 1

# Runtime configuration
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 4001 5000 9090

ENTRYPOINT ["/app/krelay"]

