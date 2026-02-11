# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Install git for potential dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod ./

# Download dependencies (currently none, but good practice)
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /mcpserver ./cmd/mcpserver

# Runtime stage
FROM alpine:3.19

# Install ca-certificates for HTTPS and bash for shell access
RUN apk add --no-cache ca-certificates bash

# Create non-root user
RUN adduser -D -g '' mcpuser

# Copy binary from builder
COPY --from=builder /mcpserver /usr/local/bin/mcpserver

# Create workspace directory
RUN mkdir -p /workspace && chown mcpuser:mcpuser /workspace

# Switch to non-root user
USER mcpuser

WORKDIR /workspace

# Default environment variables
ENV MCP_ADDR=":9876"
ENV MCP_TARGET="opencode-cli"
ENV MCP_TIMEOUT_SEC="120"

# Expose the default port
EXPOSE 9876

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:9876/health || exit 1

# Run the server
ENTRYPOINT ["/usr/local/bin/mcpserver"]
