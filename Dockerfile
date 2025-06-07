# Dockerfile
FROM golang:1.24.4-alpine AS builder

# Install git and ca-certificates
RUN apk add --no-cache git ca-certificates tzdata

# Create non-root user
RUN adduser -D -g '' appuser

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o mcp-server .

# Final stage
FROM scratch

# Copy timezone data, CA certificates, and user info
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /etc/passwd /etc/passwd

# Copy the binary
COPY --from=builder /build/mcp-server /mcp-server

# Use non-root user
USER appuser

# Expose ports
EXPOSE 8080 8081

# Run the binary
ENTRYPOINT ["/mcp-server"]
