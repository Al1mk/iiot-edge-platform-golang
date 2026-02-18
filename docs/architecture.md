# Architecture

## System Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                         FIELD / EDGE                            │
│                                                                 │
│  ┌──────────────┐   Modbus/OPC-UA   ┌─────────────────────┐   │
│  │ Field Device │ ────────────────► │  Edge Collector     │   │
│  │ (PLC/Sensor) │                   │  (edge/collector)   │   │
│  └──────────────┘                   └────────┬────────────┘   │
│                                              │ JSON over MQTT   │
└──────────────────────────────────────────────┼─────────────────┘
                                               │
                               ┌───────────────▼──────────────┐
                               │     MQTT Broker               │
                               │     (Eclipse Mosquitto)       │
                               │     tcp://mosquitto:1883      │
                               └───────────────┬──────────────┘
                                               │
                               ┌───────────────▼──────────────┐
                               │     Cloud Ingestor            │
                               │     (cloud/ingestor)          │
                               │     POST /api/v1/telemetry    │
                               └───────────────┬──────────────┘
                                               │
                        ┌──────────────────────┼─────────────────────┐
                        │                      │                     │
               ┌────────▼───────┐   ┌──────────▼──────┐   ┌────────▼───────┐
               │   Kafka Topic  │   │  TimescaleDB /  │   │  Prometheus /  │
               │  (TODO: impl)  │   │  InfluxDB       │   │  Grafana       │
               │                │   │  (TODO: impl)   │   │  (TODO: impl)  │
               └────────────────┘   └─────────────────┘   └────────────────┘
```

## Component Summary

| Component | Location | Transport | Purpose |
|-----------|----------|-----------|---------|
| Edge Collector | `edge/collector/` | MQTT only | Reads sensor data; publishes JSON to broker |
| MQTT Broker | `infra/docker/` | TCP 1883 | Message bus between edge and cloud |
| Cloud Ingestor | `cloud/ingestor/` | HTTP only | Receives and validates telemetry via REST API |

> **Scope note:** The collector publishes to MQTT only. The ingestor accepts HTTP only.
> Neither service communicates directly with the other.

## Ports Reference

| Service | Port | Protocol | Purpose |
|---------|------|----------|---------|
| mosquitto | 1883 | TCP/MQTT | MQTT broker |
| collector | 9090 | HTTP | Prometheus metrics (`/metrics`) |
| ingestor | 8080 | HTTP | REST API (`/healthz`, `POST /api/v1/telemetry`) |
| ingestor | 9091 | HTTP | Prometheus metrics (`/metrics`) |

## Data Contract

The `TelemetryReading` struct (defined in `pkg/models/telemetry.go`) is the shared data
contract. It is serialised as JSON for both MQTT payloads and HTTP request bodies.

### Example Payload

```json
{
  "device_id":      "sensor-001",
  "timestamp":      "2026-02-17T10:30:00Z",
  "temperature_c":  24.7,
  "pressure_hpa":   1013.2,
  "humidity_pct":   58.4,
  "status":         "ok"
}
```

### Field Validation Rules

| Field | Type | Constraint |
|-------|------|------------|
| `device_id` | string | Non-empty after trim; max 128 chars |
| `timestamp` | RFC 3339 | Non-zero; ingestor rejects if outside ±`MAX_TS_SKEW` (default 24 h) |
| `temperature_c` | float64 | [-50, 150] °C |
| `pressure_hpa` | float64 | [800, 1200] hPa |
| `humidity_pct` | float64 | [0, 100] % |
| `status` | string | Non-empty after trim; max 32 chars |

### HTTP Status Codes

| Status | Meaning |
|--------|---------|
| 202 | Reading accepted |
| 400 | Malformed JSON body |
| 422 | Validation failure (field ranges, timestamp skew) |

### Error Response Body

```json
{ "error": "validation_failed", "message": "humidity_pct out of range [0, 100]" }
```

Error codes: `invalid_json`, `validation_failed`.

## Deployment Options

### Option A — Docker Compose (local development)

All services run in a single bridge network (`iiot-net`). The collector connects to
Mosquitto by service name (`tcp://mosquitto:1883`). The ingestor logs received data to
stdout; storage forwarding is a TODO.

### Option B — Kubernetes (k3d / kind / production)

Manifests live in `deploy/k8s/`. Apply the Kustomize overlay in `deploy/gitops/` to set
the `iiot` namespace and common labels automatically.

---

## Security Gaps (known, pre-production)

> These are intentional trade-offs for a local/demo stack. Address them before exposing
> to any network.

1. **No MQTT authentication** — `allow_anonymous true` in `mosquitto.conf`. Production
   requires TLS and password files or X.509 client certificates.

2. **No transport encryption** — MQTT is plain TCP; HTTP has no TLS termination.
   Add a reverse proxy (Nginx/Traefik) or Mosquitto's built-in TLS listener.

3. **No ingestor authentication** — `POST /api/v1/telemetry` accepts unauthenticated
   requests. Add JWT or mTLS before network exposure.

4. **No persistent MQTT storage** — Mosquitto runs with `persistence false`. In-flight
   messages are lost on restart. Enable persistence or use a durable broker for production.

5. **Simulated sensor data** — `sampleReading()` generates random values. Real hardware
   integration (Modbus TCP, OPC-UA) is marked as TODO in the collector.
