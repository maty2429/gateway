# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install system dependencies
RUN apk add --no-cache ca-certificates git

# Cache Go dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy application source code
COPY . .

# Compile optimized static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gateway cmd/gateway/main.go

# Production stage using Google Distroless static image for minimum attack surface
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Copy executable and configuration resources
COPY --from=builder /app/gateway /app/gateway
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/configs /app/configs

# Expose primary server port and metrics server port
EXPOSE 8080 8090

ENTRYPOINT ["/app/gateway"]
