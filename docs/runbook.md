# Runbook

## Prerequisites

| Tool | Minimum Version | Install |
|------|----------------|---------|
| Go | 1.22 | https://go.dev/dl/ |
| Docker | 24.x | https://docs.docker.com/get-docker/ |
| Docker Compose | v2.x (plugin) | bundled with Docker Desktop |
| make | any | OS package manager |
| k3d *(Option B only)* | 5.x | `brew install k3d` |
| kubectl *(Option B only)* | 1.29+ | `brew install kubectl` |

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

This builds the collector and ingestor images, starts Mosquitto, and wires everything
together on the `iiot-net` bridge network. The collector waits for Mosquitto to pass its
healthcheck before connecting.

### 3. Verify health

```bash
# Ingestor HTTP API
curl http://localhost:8080/healthz
# → {"status":"ok"}

# Container health status (should show "healthy" after ~30 s)
docker inspect --format '{{.State.Health.Status}}' iiot-collector
docker inspect --format '{{.State.Health.Status}}' iiot-ingestor
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
```

### 7. Tail logs

```bash
docker logs -f iiot-ingestor
docker logs -f iiot-collector
```

### 8. Stop

```bash
make compose-down
```

---

## Option B — k3d (local Kubernetes)

### 1. Create a cluster

```bash
k3d cluster create iiot --port "8080:30080@loadbalancer"
```

### 2. Build and import images

```bash
make docker-build
make k3d-up
```

### 3. Create the namespace and apply manifests

```bash
kubectl create namespace iiot
kubectl apply -k deploy/gitops/
```

### 4. Check rollout

```bash
kubectl -n iiot rollout status deployment/iiot-ingestor
kubectl -n iiot get pods
```

### 5. Port-forward and test

```bash
kubectl -n iiot port-forward svc/iiot-ingestor 8080:80 &
curl http://localhost:8080/healthz
```

---

## Local binary build (no Docker)

Requires Go 1.22+.

```bash
# Builds bin/collector and bin/ingestor
make build

# Run ingestor (terminal 1)
./bin/ingestor

# Run collector (terminal 2) — needs a local MQTT broker
MQTT_BROKER=tcp://localhost:1883 ./bin/collector

# Healthcheck probes (exit 0 = healthy)
./bin/ingestor  -healthcheck
./bin/collector -healthcheck
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

---

## Troubleshooting

### Collector exits: `initial MQTT connect failed`

- Mosquitto is not running — start with `make compose-up` or `brew services start mosquitto`.
- `MQTT_BROKER` is wrong — default is `tcp://localhost:1883`.
- Port 1883 is firewalled — check firewall rules.

### Ingestor returns 422 on valid-looking payload

- Verify all fields are within range (see architecture.md field table).
- Check `MAX_TS_SKEW` — if your timestamp is stale beyond the limit, the request is rejected.
- Ensure `status` field is not empty.

### `docker inspect` shows `unhealthy`

- Wait for `start_period` (15 s) to elapse before declaring unhealthy.
- Check logs: `docker logs iiot-collector` / `docker logs iiot-ingestor`.
- Ensure the metrics port (9090 collector, 8080 ingestor) is listening inside the container.

### k3d image import fails

```bash
docker images | grep iiot   # verify image exists
make docker-build            # rebuild if missing
k3d image import iiot-collector:dev -c iiot
k3d image import iiot-ingestor:dev  -c iiot
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

Or remap the host port in `infra/docker/docker-compose.yml`:

```yaml
ports:
  - "9080:8080"
```
