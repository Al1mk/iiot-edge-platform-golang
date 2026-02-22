# Runbook

## Prerequisites

| Tool | Minimum Version | Install |
|------|----------------|---------|
| Go | 1.22 | https://go.dev/dl/ |
| Docker | 24.x | https://docs.docker.com/get-docker/ |
| Docker Compose | v2.x (plugin) | bundled with Docker Desktop |
| make | any | OS package manager |
| k3d | 5.x | `brew install k3d` |
| kubectl | 1.29+ | `brew install kubectl` |
| kustomize | 5.x | bundled with kubectl 1.29+ (`kubectl kustomize`) |

> **Go version:** The project uses `go 1.23` in `go.mod` and relies on Go 1.22+
> method-qualified ServeMux patterns (`"GET /path"`). Go 1.22 is the minimum.

---

## Initialize Git + first push

Run these commands once from the project root before any other workflow step.

```bash
# Initialize the repository
git init
git branch -M main

# Stage all files — bin/ and .env are already excluded by .gitignore
git add -A

# First commit
git commit -m "chore: initial commit"

# Create the remote and push (replace URL with your actual remote)
git remote add origin https://github.com/<org>/iiot-edge-platform.git
git push -u origin main
```

> **Note:** `bin/` is listed in `.gitignore` and will not be staged.
> Run `git status` before committing to confirm no build artifacts or secrets are included.

---

## Option A — Docker Compose (quickest start)

### 1. Clone and bootstrap

```bash
git clone https://github.com/alimk/iiot-edge-platform.git
cd iiot-edge-platform

# Download and verify all declared dependencies
go mod tidy
```

### 2. Start the full stack

```bash
make compose-up
```

This builds all four images (collector, ingestor, mqtt-bridge, mosquitto), and wires
everything together on the `iiot-net` bridge network. The collector and mqtt-bridge wait
for Mosquitto to pass its healthcheck before connecting.

### 3. Verify health

```bash
# Ingestor HTTP API
curl http://localhost:8080/healthz
# → {"status":"ok"}

# Container health status (should show "healthy" after ~30 s)
docker inspect --format '{{.State.Health.Status}}' iiot-collector
docker inspect --format '{{.State.Health.Status}}' iiot-ingestor
docker inspect --format '{{.State.Health.Status}}' iiot-mqtt-bridge
```

### 4. Watch live MQTT messages from the host

```bash
# Subscribe to the telemetry topic (requires mosquitto-clients installed locally)
mosquitto_sub -h localhost -p 1883 -t "iiot/telemetry" -v
```

### 5. Post telemetry manually

```bash
curl -s -X POST http://localhost:8080/api/v1/telemetry \
  -H "Content-Type: application/json" \
  -d '{
    "device_id":     "test-001",
    "timestamp":     "2026-02-17T10:00:00Z",
    "temperature_c": 22.5,
    "pressure_hpa":  1012.0,
    "humidity_pct":  55.0,
    "status":        "ok"
  }'
# → {"result":"accepted"}
```

Validation rejection example:

```bash
curl -s -X POST http://localhost:8080/api/v1/telemetry \
  -H "Content-Type: application/json" \
  -d '{"device_id":"","timestamp":"2026-02-17T10:00:00Z","temperature_c":22.5,"pressure_hpa":1012.0,"humidity_pct":55.0,"status":"ok"}'
# → 422 {"error":"validation_failed","message":"device_id is required"}
```

### 6. Check Prometheus metrics

```bash
# Ingestor metrics
curl http://localhost:9091/metrics | grep iiot_http_requests_total

# Collector metrics
curl http://localhost:9090/metrics | grep iiot_publish

# Bridge metrics
curl http://localhost:9092/metrics | grep bridge_
```

### 7. Tail logs

```bash
docker logs -f iiot-ingestor
docker logs -f iiot-collector
docker logs -f iiot-mqtt-bridge
```

### 8. Stop

```bash
make compose-down
```

---

## Option B — k3d (local Kubernetes)

This section describes the full end-to-end local Kubernetes workflow using k3d.
All four components (mosquitto, collector, mqtt-bridge, ingestor) are deployed
into the `iiot` namespace via kustomize.

### Manifest layout

`deploy/kustomize/base/` is the **single source of truth** for all Kubernetes YAML.
Overlays layer on top of it; nothing else in the repo duplicates these manifests.

```
deploy/
├── kustomize/
│   ├── base/                   # Canonical manifests — one file per resource
│   │   ├── namespace.yaml
│   │   ├── mosquitto-configmap.yaml
│   │   ├── mosquitto-deployment.yaml
│   │   ├── mosquitto-service.yaml
│   │   ├── collector-deployment.yaml
│   │   ├── collector-service.yaml
│   │   ├── mqtt-bridge-deployment.yaml
│   │   ├── mqtt-bridge-service.yaml
│   │   ├── ingestor-deployment.yaml
│   │   ├── ingestor-service.yaml
│   │   └── kustomization.yaml
│   └── overlays/local/         # k3d-specific: dev image tags + NodePort patch
│       ├── ingestor-nodeport-patch.yaml
│       └── kustomization.yaml
└── gitops/
    └── kustomization.yaml      # FluxCD entry-point (delegates to overlays/local)
```

To validate manifests render correctly without a cluster:

```bash
make manifests-lint
```

### 1. Create the k3d cluster

```bash
make k3d-cluster-up
```

This creates a cluster named `iiot` with:
- Host port `8080` → NodePort `30080` (ingestor HTTP API, reachable at `http://localhost:8080`)
- 1 agent node

Metrics ports are **not** mapped via NodePort. Access them with `kubectl port-forward` (see step 6).

Skip if the cluster already exists — the target is idempotent.

### 2. Build images and load them into k3d

```bash
make k3d-load
```

This runs `docker build` for all three Go services and imports the resulting images
directly into the k3d containerd store via `k3d image import`. No registry is required.

**Tag convention:** `VERSION` defaults to `dev`, which matches the `newTag: dev` pins in
`deploy/kustomize/overlays/local/kustomization.yaml`. The two are always in sync without
any extra flags.

For staging or production, create a dedicated overlay (e.g.
`deploy/kustomize/overlays/staging/`) that pins the desired image tags. Any change to an
overlay is a source-controlled file that must be committed — do not edit overlay files
in-place without committing the result.

### 3. Deploy to the cluster

```bash
make k3d-deploy
```

Validates manifests with `make manifests-lint`, applies `deploy/kustomize/overlays/local`,
and waits for the collector, ingestor, and mqtt-bridge rollouts to complete (90 s timeout each).

**Iterative dev workflow** — after the cluster exists, skip cluster recreation:

```bash
make k3d-redeploy   # k3d-load + k3d-deploy, no cluster recreate
```

To run all three steps from scratch (idempotent):

```bash
make k3d-up
```

### 4. Verify pods and services are running

Run these commands to confirm the cluster state:

```bash
kubectl -n iiot get pods,svc
```

Expected output (all pods `Running`, `1/1` or `2/2` ready):

```
NAME                              READY   STATUS    RESTARTS   AGE
pod/collector-xxx                 1/1     Running   0          60s
pod/ingestor-xxx                  1/1     Running   0          60s
pod/ingestor-yyy                  1/1     Running   0          60s
pod/mqtt-bridge-xxx               1/1     Running   0          60s
pod/mosquitto-xxx                 1/1     Running   0          60s

NAME                      TYPE        CLUSTER-IP    PORT(S)
svc/collector-metrics     ClusterIP   10.x.x.x      9090/TCP
svc/ingestor              NodePort    10.x.x.x      8080:30080/TCP, 9091/TCP
svc/mqtt-bridge-metrics   ClusterIP   10.x.x.x      9092/TCP
svc/mosquitto             ClusterIP   10.x.x.x      1883/TCP
```

### 5. Verify ingestor HTTP API (via NodePort)

The ingestor is exposed on host port 8080 via the k3d load-balancer:

```bash
curl http://localhost:8080/healthz
# → {"status":"ok"}

curl -s -X POST http://localhost:8080/api/v1/telemetry \
  -H "Content-Type: application/json" \
  -d '{
    "device_id":     "k8s-test-001",
    "timestamp":     "2026-02-18T10:00:00Z",
    "temperature_c": 22.5,
    "pressure_hpa":  1012.0,
    "humidity_pct":  55.0,
    "status":        "ok"
  }'
# → {"result":"accepted"}
```

### 6. Port-forward metrics ports and run smoke checks

The recommended workflow runs all validation in three commands:

```bash
make k3d-metrics   # start port-forwards (idempotent — safe to re-run)
make k3d-smoke     # healthz + all three metrics checks
make k3d-metrics-down
```

`make k3d-metrics` forwards to conflict-free local ports and writes each tunnel's
output to a PID-tracked log file under `/tmp/`:

| Service | Local URL | PID file | Log |
|---------|-----------|----------|-----|
| collector | `http://localhost:19090/metrics` | `/tmp/pf-collector.pid` | `/tmp/pf-collector.log` |
| ingestor | `http://localhost:19091/metrics` | `/tmp/pf-ingestor.pid` | `/tmp/pf-ingestor.log` |
| bridge | `http://localhost:19092/metrics` | `/tmp/pf-bridge.pid` | `/tmp/pf-bridge.log` |

Both `make k3d-metrics` and `make k3d-smoke` are idempotent: if a port-forward is already
running (PID file present and process alive) it will not start a duplicate.

`make k3d-smoke` checks:

1. `GET /healthz` returns `{"status":"ok"}`
2. `iiot_publish_success_total` present in collector metrics
3. `bridge_forward_success_total` present in bridge metrics
4. `iiot_http_requests_total` present in ingestor metrics

If any check fails it prints which one failed and exits non-zero. On success it prints `SMOKE OK`.

> **macOS note:** after `make k3d-metrics-down`, `lsof` may show `com.docke` holding a port.
> This is Docker Desktop's userspace proxy and is normal — it does not mean a port-forward is
> still running.

### 7. Verify end-to-end telemetry flow

The collector publishes to Mosquitto every 5 s. The bridge subscribes and forwards
to the ingestor. Poll the bridge counter to confirm the full pipeline is flowing:

```bash
# Option 1 — watch (GNU coreutils; not available on stock macOS):
watch -n 5 "curl -s http://localhost:19092/metrics | grep bridge_forward_success_total"

# Option 2 — portable loop (works on macOS zsh and Linux):
while true; do
  curl -s http://localhost:19092/metrics | grep bridge_forward_success_total
  sleep 5
done

# Ingestor logs should show 'received telemetry' entries
kubectl -n iiot logs -f -l app.kubernetes.io/name=ingestor --prefix
```

> **Note on exec probes:** Kubernetes `exec` probes do not require a shell — they run
> the command array directly via the container runtime, the same way Docker's exec-form
> `HEALTHCHECK CMD ["/app/binary", "-healthcheck"]` works. The Go service containers use
> HTTP probes (`GET /metrics` and `GET /healthz`) as a straightforward health signal that
> avoids coupling the probe implementation to the binary's `-healthcheck` flag. Either
> approach is valid; HTTP probes are used here for simplicity.

### 8. E2E verification

Run the full end-to-end check in one command:

```bash
make k3d-e2e
```

This target:
1. Runs `make k3d-up` (idempotent — skips cluster/image steps if already done)
2. Starts metrics port-forwards via `make k3d-metrics`
3. POSTs a known telemetry payload to `/api/v1/telemetry` and asserts HTTP 202
4. GETs `/api/v1/telemetry/last` and asserts the device ID is present
5. Reads `iiot_last_telemetry_timestamp_seconds` from ingestor metrics and asserts it is > 0
6. Always runs `make k3d-metrics-down` on exit (via shell trap), even if a step fails

On success it prints `E2E OK`.

To additionally verify SQLite persistence (query endpoints and DB metrics):

```bash
make k3d-e2e-db
```

This target (cluster must already be up) checks:
1. POST telemetry for device `e2e-db-test` → HTTP 202
2. `GET /api/v1/telemetry/last?device_id=e2e-db-test` → HTTP 200 with `received_at_unix`
3. `GET /api/v1/telemetry/recent?device_id=e2e-db-test&limit=1` → HTTP 200 with the record
4. `iiot_db_up` metric is `1` in ingestor metrics

On success it prints `E2E-DB OK`.

> **Session affinity:** the ingestor `Service` sets `sessionAffinity: ClientIP` (3-hour timeout).
> This pins all requests from the same source IP to the same replica, so a POST followed
> immediately by a GET `/last` always hits the same in-memory store. The e2e target seeds
> affinity with a `GET /healthz` before the POST to ensure the pin is established.

> **SQLite persistence:** telemetry is written to a SQLite database at `INGESTOR_DB_PATH`
> (default `/tmp/iiot.db`). In Kubernetes, `/tmp` is backed by an `emptyDir` volume — data
> is durable for the pod's lifetime but lost on restart. The query endpoints (`/last`,
> `/recent`) are backed by the DB; the in-memory `lastStore` is kept as a fast fallback only.

### 9. Tail all logs at once

```bash
make k3d-logs
```

### 10. Tear down the cluster

```bash
make k3d-down
```

---

## Running tests

Run before every push to catch regressions and data races:

```bash
make test        # go test ./... -v -race -timeout 60s
make test-race   # explicit alias for the same command (useful in CI scripts)
```

Coverage includes: `GET /api/v1/telemetry/last` and `/recent` (backed by in-memory SQLite),
the Prometheus gauge, `routeLabel` stability, and a 50-goroutine concurrent-POST test that
surfaces mutex omissions under `-race`. Each test gets its own isolated `:memory:` SQLite
database — no filesystem access, no cross-test bleed.

---

## Local binary build (no Docker)

Requires Go 1.22+.

```bash
# Builds bin/collector, bin/ingestor, and bin/mqtt-bridge
make build

# Run ingestor (terminal 1)
./bin/ingestor

# Run collector (terminal 2) — needs a local MQTT broker
MQTT_BROKER=tcp://localhost:1883 ./bin/collector

# Run mqtt-bridge (terminal 3) — needs mosquitto and ingestor
MQTT_BROKER=tcp://localhost:1883 \
INGESTOR_URL=http://localhost:8080/api/v1/telemetry \
./bin/mqtt-bridge

# Healthcheck probes (exit 0 = healthy)
./bin/ingestor    -healthcheck
./bin/collector   -healthcheck
./bin/mqtt-bridge -healthcheck
```

---

## Environment Variables

### Collector

| Variable | Default | Description |
|----------|---------|-------------|
| `MQTT_BROKER` | `tcp://localhost:1883` | MQTT broker URL |
| `MQTT_CLIENT_ID` | `iiot-collector-1` | MQTT client identifier |
| `MQTT_TOPIC` | `iiot/telemetry` | Topic to publish to |
| `DEVICE_ID` | `sensor-001` | Device identifier included in every reading |
| `METRICS_ADDR` | `:9090` | Address for the Prometheus metrics server |
| `PUBLISH_INTERVAL` | `5s` | How often to publish a reading (Go duration string) |

### Ingestor

| Variable | Default | Description |
|----------|---------|-------------|
| `MAX_TS_SKEW` | `24h` | Maximum allowed timestamp skew relative to server time |
| `INGESTOR_DB_PATH` | `/tmp/iiot.db` | Path to the SQLite database file. In Kubernetes the `/tmp` directory is backed by an emptyDir volume so data is lost on pod restart. Mount a PersistentVolume here for durable storage. |

### MQTT Bridge

| Variable | Default | Description |
|----------|---------|-------------|
| `MQTT_BROKER` | `tcp://mosquitto:1883` | MQTT broker URL |
| `MQTT_TOPIC` | `iiot/telemetry` | Topic to subscribe to |
| `INGESTOR_URL` | `http://ingestor:8080/api/v1/telemetry` | Ingestor POST endpoint |
| `METRICS_ADDR` | `:9092` | Address for the Prometheus metrics server |
| `BRIDGE_WORKERS` | `16` | Number of parallel forwarding workers |
| `BRIDGE_QUEUE_SIZE` | `1000` | Buffered channel depth |
| `SHUTDOWN_TIMEOUT` | `10s` | Max time to drain queue on SIGTERM |

---

## GitOps (FluxCD) — optional

The `deploy/gitops/kustomization.yaml` file is a thin wrapper that delegates
to `deploy/kustomize/overlays/local`. It is the intended target for a FluxCD
`Kustomization` custom resource.

### Bootstrap guide (docs only — Flux is not required to run the stack)

1. Install Flux CLI:

```bash
brew install fluxcd/tap/flux
```

2. Bootstrap Flux into your cluster, pointing at your Git repository:

```bash
flux bootstrap github \
  --owner=<your-github-org> \
  --repository=iiot-edge-platform \
  --branch=main \
  --path=deploy/gitops \
  --personal
```

3. Flux will reconcile `deploy/gitops/kustomization.yaml` on every push to `main`.
   The kustomization delegates to `deploy/kustomize/overlays/local`, so the same
   manifests used locally are applied by Flux.

4. To promote to a production overlay, create `deploy/kustomize/overlays/prod/`,
   update `deploy/gitops/kustomization.yaml` to point there, and add image update
   automation for production image tags.

> **Note:** For production use, replace the `dev` image tags with immutable digest
> references (`image@sha256:...`) and store sensitive config in Kubernetes Secrets
> or an external secrets manager (e.g. External Secrets Operator).

---

## Troubleshooting

### Collector exits: `initial MQTT connect failed`

- Mosquitto is not running — run `make compose-up` or check the mosquitto pod: `kubectl -n iiot logs -l app.kubernetes.io/name=mosquitto`.
- `MQTT_BROKER` is wrong — in k8s it must be `tcp://mosquitto.iiot.svc.cluster.local:1883`.
- Port 1883 is firewalled — check firewall rules.

### Ingestor returns 422 on valid-looking payload

- Verify all fields are within range (see architecture.md field table).
- Check `MAX_TS_SKEW` — if your timestamp is stale beyond the limit, the request is rejected.
- Ensure `status` field is not empty.

### Pods stuck in `Pending`

- Run `kubectl -n iiot describe pod <pod-name>` and look for `Events`.
- Common causes: image not loaded into k3d (`make k3d-load`), resource limits exceeded.

### `ImagePullBackOff` in k3d

```bash
# Confirm the image exists locally
docker images | grep iiot

# Reload into k3d
make k3d-load
```

`imagePullPolicy: IfNotPresent` is set on all Go service containers. As long as
`k3d image import` succeeds, the image will be used from the containerd store.

### k3d image import fails

```bash
docker images | grep iiot   # verify image was built
make docker-build            # rebuild if missing
k3d image import iiot-collector:dev   -c iiot
k3d image import iiot-ingestor:dev    -c iiot
k3d image import iiot-mqtt-bridge:dev -c iiot
```

### go.sum out of date

```bash
go mod tidy
```

### Port 8080 already in use

```bash
lsof -i :8080        # find the process
kill -9 <PID>
```

Or create the k3d cluster on a different host port:

```bash
k3d cluster create iiot --port "9080:30080@loadbalancer"
```

Then access the ingestor at `http://localhost:9080`.
