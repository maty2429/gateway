.PHONY: all build run test clean proto docker-build

all: build

build:
	@go build -o bin/gateway cmd/gateway/main.go

run: build
	@./bin/gateway

test:
	go test -v ./...

clean:
	rm -rf bin/

proto:
	protoc --go_out=. --go-grpc_out=. proto/*.proto

docker-build:
	docker build -f deployments/Dockerfile -t api-gateway:latest ..
