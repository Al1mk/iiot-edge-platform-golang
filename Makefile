# Local dev default. CI/release builds override this:
#   make k3d-load VERSION=v1.2.3
# The kustomize local overlay always pins newTag: dev, so the local workflow
# requires VERSION=dev (the default). Override only when cutting a release.
VERSION ?= dev
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"

COLLECTOR_BIN  := bin/collector
INGESTOR_BIN   := bin/ingestor
BRIDGE_BIN     := bin/mqtt-bridge

K3D_CLUSTER    := iiot
# Single source of truth for all Kubernetes manifests.
# Overlays layer on top; nothing else in this repo should reference raw YAML directly.
KUSTOMIZE_BASE    := deploy/kustomize/base
KUSTOMIZE_OVERLAY := deploy/kustomize/overlays/local

.PHONY: fmt lint test build \
        docker-build \
        manifests-lint \
        k3d-cluster-up k3d-up k3d-down k3d-load k3d-deploy k3d-redeploy k3d-logs \
        k3d-metrics k3d-metrics-down \
        compose-up compose-down \
        clean help

# ---------------------------------------------------------------------------
# Development
# ---------------------------------------------------------------------------

## fmt: Format all Go source files
fmt:
	go fmt ./...

## lint: Run golangci-lint (must be installed separately)
lint:
	golangci-lint run ./...

## test: Run all unit tests with race detector
test:
	go test ./... -v -race -timeout 60s

## build: Compile all binaries to bin/
build:
	mkdir -p bin
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(COLLECTOR_BIN) ./edge/collector/...
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(INGESTOR_BIN)  ./cloud/ingestor/...
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BRIDGE_BIN)    ./cloud/mqtt-bridge/...

# ---------------------------------------------------------------------------
# Kubernetes manifests
# ---------------------------------------------------------------------------

## manifests-lint: Validate kustomize builds render without errors (no cluster needed).
##   Run this in CI to catch broken YAML or missing resources early.
manifests-lint:
	kubectl kustomize $(KUSTOMIZE_OVERLAY) > /dev/null
	kubectl kustomize deploy/gitops         > /dev/null
	@echo "kustomize build OK"

# ---------------------------------------------------------------------------
# Docker
# ---------------------------------------------------------------------------

## docker-build: Build all three Go service images tagged with VERSION
docker-build:
	docker build -f edge/collector/Dockerfile    -t iiot-collector:$(VERSION)   .
	docker build -f cloud/ingestor/Dockerfile    -t iiot-ingestor:$(VERSION)    .
	docker build -f cloud/mqtt-bridge/Dockerfile -t iiot-mqtt-bridge:$(VERSION) .

# ---------------------------------------------------------------------------
# Docker Compose
# ---------------------------------------------------------------------------

## compose-up: Build and start the full stack with Docker Compose
compose-up:
	docker compose -f infra/docker/docker-compose.yml up --build -d

## compose-down: Stop and remove all Compose services
compose-down:
	docker compose -f infra/docker/docker-compose.yml down

# ---------------------------------------------------------------------------
# k3d — local Kubernetes cluster
# ---------------------------------------------------------------------------

## k3d-cluster-up: Create the k3d cluster (skips if it already exists).
##   Maps host :8080 → NodePort 30080 (ingestor HTTP API).
##   Metrics ports are NOT exposed via NodePort; use kubectl port-forward instead.
k3d-cluster-up:
	@if k3d cluster list | grep -q "^$(K3D_CLUSTER) "; then \
	  echo "k3d cluster '$(K3D_CLUSTER)' already exists — skipping create"; \
	else \
	  k3d cluster create $(K3D_CLUSTER) \
	    --port "8080:30080@loadbalancer" \
	    --agents 1; \
	fi

## k3d-down: Delete the k3d cluster
k3d-down:
	k3d cluster delete $(K3D_CLUSTER)

## k3d-load: Build images and import them into the k3d cluster.
##   Uses the current VERSION tag. Run this after every code change.
k3d-load: docker-build
	k3d image import iiot-collector:$(VERSION)   -c $(K3D_CLUSTER)
	k3d image import iiot-ingestor:$(VERSION)    -c $(K3D_CLUSTER)
	k3d image import iiot-mqtt-bridge:$(VERSION) -c $(K3D_CLUSTER)

## k3d-deploy: Validate manifests, then apply the kustomize local overlay
k3d-deploy: manifests-lint
	kubectl apply -k $(KUSTOMIZE_OVERLAY)
	kubectl -n iiot rollout status deployment/collector   --timeout=90s
	kubectl -n iiot rollout status deployment/ingestor    --timeout=90s
	kubectl -n iiot rollout status deployment/mqtt-bridge --timeout=90s

## k3d-up: Full workflow — create cluster, load images, deploy (idempotent)
k3d-up: k3d-cluster-up k3d-load k3d-deploy

## k3d-redeploy: Rebuild images and redeploy without recreating the cluster.
##   Use this for iterative code changes after the cluster already exists.
k3d-redeploy: k3d-load k3d-deploy

## k3d-metrics: Start port-forwards for all three metrics endpoints.
##   Logs written to /tmp/pf-{collector,bridge,ingestor}.log
##   Ports: collector=19090, ingestor=19091, bridge=19092
k3d-metrics:
	@kubectl -n iiot port-forward svc/collector-metrics   19090:9090 >/tmp/pf-collector.log 2>&1 &
	@kubectl -n iiot port-forward svc/mqtt-bridge-metrics 19092:9092 >/tmp/pf-bridge.log   2>&1 &
	@kubectl -n iiot port-forward svc/ingestor            19091:9091 >/tmp/pf-ingestor.log 2>&1 &
	@echo "Port-forwards started."
	@echo "  collector  → http://localhost:19090/metrics  (log: /tmp/pf-collector.log)"
	@echo "  ingestor   → http://localhost:19091/metrics  (log: /tmp/pf-ingestor.log)"
	@echo "  bridge     → http://localhost:19092/metrics  (log: /tmp/pf-bridge.log)"

## k3d-metrics-down: Stop the port-forwards started by k3d-metrics.
k3d-metrics-down:
	@pkill -f "kubectl -n iiot port-forward svc/collector-metrics 19090:9090"   || true
	@pkill -f "kubectl -n iiot port-forward svc/mqtt-bridge-metrics 19092:9092" || true
	@pkill -f "kubectl -n iiot port-forward svc/ingestor 19091:9091"            || true
	@echo "Port-forwards stopped. Remaining listeners on 19090/19091/19092:"
	@lsof -nP -iTCP:19090 -sTCP:LISTEN 2>/dev/null || true
	@lsof -nP -iTCP:19091 -sTCP:LISTEN 2>/dev/null || true
	@lsof -nP -iTCP:19092 -sTCP:LISTEN 2>/dev/null || true

## k3d-logs: Tail logs from all iiot pods (Ctrl-C to exit)
k3d-logs:
	kubectl -n iiot logs -f --prefix \
	  -l app.kubernetes.io/part-of=iiot-edge-platform \
	  --max-log-requests 10

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------

## clean: Remove build artifacts
clean:
	rm -rf bin/

## help: Show this help message
help:
	@echo "Available targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/  /'
