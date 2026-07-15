# Build with the workspace root as context:
# docker build -f gateway/deployments/Dockerfile -t api-gateway:latest .
FROM golang:1.26.4-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /workspace

COPY contracts/go.mod contracts/go.sum ./contracts/
COPY gateway/go.mod gateway/go.sum ./gateway/
RUN --mount=type=cache,target=/go/pkg/mod cd gateway && go mod download

COPY contracts ./contracts
COPY gateway ./gateway
WORKDIR /workspace/gateway
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go test -count=1 ./...
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" -o /gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /gateway /app/gateway
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /workspace/gateway/configs /app/configs
EXPOSE 8080 8090
ENTRYPOINT ["/app/gateway"]
