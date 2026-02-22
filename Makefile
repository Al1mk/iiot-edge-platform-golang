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

.PHONY: fmt lint test test-race build \
        docker-build \
        manifests-lint \
        k3d-guard-docker k3d-guard-kube \
        k3d-cluster-up k3d-up k3d-down k3d-load k3d-deploy k3d-redeploy k3d-logs \
        k3d-metrics k3d-metrics-down k3d-smoke k3d-e2e \
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

## test-race: Explicit alias for test — always runs with -race (useful for CI)
test-race: test

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
# k3d — guards
# ---------------------------------------------------------------------------

## k3d-guard-docker: Fail fast if Docker daemon is not reachable.
k3d-guard-docker:
	@docker info >/dev/null 2>&1 || { \
	  echo "Docker is not running. Start Docker Desktop and retry."; \
	  exit 1; \
	}

## k3d-guard-kube: Fail fast if kubectl cannot reach the k3d cluster.
k3d-guard-kube:
	@ctx=$$(kubectl config current-context 2>/dev/null) || ctx=""; \
	case "$$ctx" in \
	  k3d-$(K3D_CLUSTER)*) ;; \
	  *) echo "Kubernetes context is not ready for k3d cluster '$(K3D_CLUSTER)'. Run: make k3d-up"; exit 1 ;; \
	esac
	@kubectl cluster-info >/dev/null 2>&1 || { \
	  echo "Kubernetes context is not ready for k3d cluster '$(K3D_CLUSTER)'. Run: make k3d-up"; \
	  exit 1; \
	}
	@kubectl get nodes >/dev/null 2>&1 || { \
	  echo "Kubernetes context is not ready for k3d cluster '$(K3D_CLUSTER)'. Run: make k3d-up"; \
	  exit 1; \
	}

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

## k3d-metrics: Start port-forwards for all three metrics endpoints (idempotent via PID files).
##   PID files: /tmp/pf-{collector,ingestor,bridge}.pid
##   Logs:      /tmp/pf-{collector,ingestor,bridge}.log
##   Ports:     collector=19090, ingestor=19091, bridge=19092
k3d-metrics: k3d-guard-docker k3d-guard-kube
	@_pf_start() { \
	  name=$$1; svc=$$2; local_port=$$3; remote_port=$$4; \
	  pidfile="/tmp/pf-$${name}.pid"; \
	  logfile="/tmp/pf-$${name}.log"; \
	  if [ -f "$$pidfile" ]; then \
	    pid=$$(cat "$$pidfile"); \
	    if kill -0 "$$pid" 2>/dev/null; then \
	      echo "  $$name port-forward already running (pid $$pid)"; \
	      return 0; \
	    fi; \
	    rm -f "$$pidfile"; \
	  fi; \
	  kubectl -n iiot port-forward "$$svc" "$${local_port}:$${remote_port}" \
	    >"$$logfile" 2>&1 & \
	  echo $$! >"$$pidfile"; \
	  echo "  $$name → http://localhost:$${local_port}/metrics  (pid $$(cat $$pidfile), log: $$logfile)"; \
	}; \
	_pf_start collector  svc/collector-metrics   19090 9090; \
	_pf_start ingestor   svc/ingestor            19091 9091; \
	_pf_start bridge     svc/mqtt-bridge-metrics 19092 9092; \
	echo "Port-forwards started."

## k3d-metrics-down: Stop port-forwards started by k3d-metrics (PID-file aware, idempotent).
k3d-metrics-down:
	@_pf_stop() { \
	  name=$$1; pattern=$$2; \
	  pidfile="/tmp/pf-$${name}.pid"; \
	  if [ -f "$$pidfile" ]; then \
	    pid=$$(cat "$$pidfile"); \
	    if kill -0 "$$pid" 2>/dev/null; then \
	      kill "$$pid" 2>/dev/null || true; \
	    fi; \
	    rm -f "$$pidfile"; \
	  else \
	    pkill -f "$$pattern" 2>/dev/null || true; \
	  fi; \
	}; \
	_pf_stop collector "kubectl -n iiot port-forward svc/collector-metrics 19090:9090"; \
	_pf_stop ingestor  "kubectl -n iiot port-forward svc/ingestor 19091:9091"; \
	_pf_stop bridge    "kubectl -n iiot port-forward svc/mqtt-bridge-metrics 19092:9092"; \
	echo "Port-forwards stopped."

## k3d-smoke: End-to-end health check: API, then all three metrics endpoints.
k3d-smoke: k3d-guard-docker k3d-guard-kube
	@echo "--- smoke: ingestor healthz ---"
	@result=$$(curl -fsS http://localhost:8080/healthz 2>&1) || { \
	  echo "FAIL: healthz — $$result"; exit 1; \
	}; \
	case "$$result" in \
	  *'"status":"ok"'*) echo "  healthz OK" ;; \
	  *) echo "FAIL: healthz response did not contain \"status\":\"ok\" — got: $$result"; exit 1 ;; \
	esac
	@echo "--- smoke: ensure metrics port-forwards are running ---"
	@_ensure_pf() { \
	  name=$$1; svc=$$2; local_port=$$3; remote_port=$$4; \
	  pidfile="/tmp/pf-$${name}.pid"; \
	  if [ -f "$$pidfile" ] && kill -0 "$$(cat $$pidfile)" 2>/dev/null; then \
	    echo "  $$name port-forward already running"; \
	  else \
	    [ -f "$$pidfile" ] && rm -f "$$pidfile"; \
	    kubectl -n iiot port-forward "$$svc" "$${local_port}:$${remote_port}" \
	      >"/tmp/pf-$${name}.log" 2>&1 & \
	    echo $$! >"$$pidfile"; \
	    echo "  $$name port-forward started (pid $$(cat $$pidfile))"; \
	    sleep 2; \
	  fi; \
	}; \
	_ensure_pf collector svc/collector-metrics   19090 9090; \
	_ensure_pf ingestor  svc/ingestor            19091 9091; \
	_ensure_pf bridge    svc/mqtt-bridge-metrics 19092 9092
	@echo "--- smoke: collector metrics ---"
	@out=$$(curl -fsS http://localhost:19090/metrics 2>&1) || { \
	  echo "FAIL: collector metrics unreachable — $$out"; exit 1; \
	}; \
	echo "$$out" | grep -q "iiot_publish_success_total" || { \
	  echo "FAIL: iiot_publish_success_total not found in collector metrics"; exit 1; \
	}; \
	echo "  iiot_publish_success_total OK"
	@echo "--- smoke: bridge metrics ---"
	@out=$$(curl -fsS http://localhost:19092/metrics 2>&1) || { \
	  echo "FAIL: bridge metrics unreachable — $$out"; exit 1; \
	}; \
	echo "$$out" | grep -q "bridge_forward_success_total" || { \
	  echo "FAIL: bridge_forward_success_total not found in bridge metrics"; exit 1; \
	}; \
	echo "  bridge_forward_success_total OK"
	@echo "--- smoke: ingestor metrics ---"
	@out=$$(curl -fsS http://localhost:19091/metrics 2>&1) || { \
	  echo "FAIL: ingestor metrics unreachable — $$out"; exit 1; \
	}; \
	echo "$$out" | grep -q "iiot_http_requests_total" || { \
	  echo "FAIL: iiot_http_requests_total not found in ingestor metrics"; exit 1; \
	}; \
	echo "  iiot_http_requests_total OK"
	@echo "SMOKE OK"

## k3d-e2e: Full end-to-end verification: deploy, POST telemetry, GET last, check gauge.
##   Starts metrics port-forwards automatically and always stops them on exit (trap).
k3d-e2e: k3d-guard-docker k3d-guard-kube
	@echo "--- e2e: ensure cluster and services are up ---"
	@$(MAKE) k3d-up
	@echo "--- e2e: start metrics port-forwards ---"
	@$(MAKE) k3d-metrics
	@echo "--- e2e: running checks (metrics-down runs on exit) ---"
	@trap '$(MAKE) k3d-metrics-down' EXIT; \
	set -e; \
	\
	echo "--- e2e: seed ClientIP affinity ---"; \
	curl -s http://localhost:8080/healthz >/dev/null; \
	\
	echo "--- e2e: POST telemetry ---"; \
	now=$$(date -u +"%Y-%m-%dT%H:%M:%SZ"); \
	payload="{\"device_id\":\"e2e-test\",\"timestamp\":\"$${now}\",\"temperature_c\":21.5,\"pressure_hpa\":1013.0,\"humidity_pct\":50.0,\"status\":\"ok\"}"; \
	http_code=$$(curl -s -o /tmp/e2e-post.out -w "%{http_code}" \
	  -X POST http://localhost:8080/api/v1/telemetry \
	  -H "Content-Type: application/json" \
	  -d "$$payload"); \
	echo "  POST /api/v1/telemetry → HTTP $${http_code}  body: $$(cat /tmp/e2e-post.out)"; \
	[ "$$http_code" = "202" ] || { echo "FAIL: expected 202, got $${http_code}"; exit 1; }; \
	echo "  POST OK"; \
	\
	echo "--- e2e: GET /api/v1/telemetry/last ---"; \
	found=0; \
	for _retry in 1 2 3; do \
	  http_code=$$(curl -s -o /tmp/e2e-last.out -w "%{http_code}" \
	    http://localhost:8080/api/v1/telemetry/last); \
	  body=$$(cat /tmp/e2e-last.out); \
	  [ "$$http_code" = "200" ] && echo "$$body" | grep -q '"e2e-test"' \
	    && { found=1; break; }; \
	  sleep 0.2; \
	done; \
	echo "  GET /api/v1/telemetry/last → HTTP $${http_code}  body: $${body}"; \
	[ "$$found" = "1" ] || { echo "FAIL: device_id e2e-test not found in GET /api/v1/telemetry/last"; exit 1; }; \
	echo "$$body" | grep -q '"received_at"' || { echo "FAIL: received_at not found in response"; exit 1; }; \
	echo "  GET last OK"; \
	\
	echo "--- e2e: gauge iiot_last_telemetry_timestamp_seconds ---"; \
	gauge=$$(curl -s http://localhost:19091/metrics | grep "^iiot_last_telemetry_timestamp_seconds " | awk '{print $$2}'); \
	echo "  iiot_last_telemetry_timestamp_seconds = $${gauge}"; \
	[ -n "$$gauge" ] && [ "$$(echo "$$gauge > 0" | awk '{print ($$1 > 0) ? 1 : 0}')" = "1" ] \
	  || { echo "FAIL: gauge is missing or zero"; exit 1; }; \
	echo "  gauge OK"; \
	\
	echo "E2E OK"

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
