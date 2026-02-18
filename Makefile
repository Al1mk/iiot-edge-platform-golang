VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"

COLLECTOR_BIN := bin/collector
INGESTOR_BIN  := bin/ingestor

.PHONY: fmt lint test build docker-build kind-up k3d-up compose-up compose-down clean help

## fmt: Format all Go source files
fmt:
	go fmt ./...

## lint: Run golangci-lint (must be installed separately)
lint:
	golangci-lint run ./...

## test: Run all unit tests
test:
	go test ./... -v -race -timeout 60s

## build: Compile collector and ingestor binaries to bin/
build:
	mkdir -p bin
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(COLLECTOR_BIN) ./edge/collector/...
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(INGESTOR_BIN)  ./cloud/ingestor/...

## docker-build: Build Docker images for collector and ingestor
docker-build:
	docker build -f edge/collector/Dockerfile  -t iiot-collector:$(VERSION) .
	docker build -f cloud/ingestor/Dockerfile  -t iiot-ingestor:$(VERSION)  .

## kind-up: Load images into a local kind cluster
kind-up:
	kind load docker-image iiot-collector:$(VERSION)
	kind load docker-image iiot-ingestor:$(VERSION)

## k3d-up: Import images into a local k3d cluster
k3d-up:
	k3d image import iiot-collector:$(VERSION)
	k3d image import iiot-ingestor:$(VERSION)

## compose-up: Start the full stack with Docker Compose
compose-up:
	docker compose -f infra/docker/docker-compose.yml up --build -d

## compose-down: Stop and remove all Compose services
compose-down:
	docker compose -f infra/docker/docker-compose.yml down

## clean: Remove build artifacts
clean:
	rm -rf bin/

## help: Show this help message
help:
	@echo "Available targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/  /'
