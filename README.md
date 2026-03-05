# IIoT Edge Platform

A lightweight Industrial IoT data pipeline built in Go, deployed on Kubernetes (k3s), and designed to survive production constraints — no domain, no managed cloud, no magic.

Sensor readings flow from a simulated field device → MQTT broker → bridge → HTTP ingestor → SQLite, all observable via Prometheus metrics and accessible through a Traefik ingress with BasicAuth and self-signed TLS.

> Contact: alimk1752@gmail.com

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                       EDGE / FIELD                       │
│                                                         │
│  ┌──────────────┐  simulated readings  ┌─────────────┐  │
│  │  Field Device│ ──────────────────► │  Collector  │  │
│  │ (PLC/Sensor) │                     │  (Go binary)│  │
│  └──────────────┘                     └──────┬──────┘  │
│                                              │ JSON/MQTT │
└──────────────────────────────────────────────┼──────────┘
                                               │
                                 ┌─────────────▼──────────┐
                                 │   Eclipse Mosquitto     │
                                 │   MQTT broker :1883     │
                                 └─────────────┬──────────┘
                                               │
                                 ┌─────────────▼──────────┐
                                 │   MQTT Bridge (Go)      │
                                 │   subscriber→forwarder  │
                                 └─────────────┬──────────┘
                                               │ HTTP POST
                                 ┌─────────────▼──────────┐
                                 │   Ingestor (Go)         │
                                 │   :8080 REST API        │
                                 │   :9091 Prometheus      │
                                 │   SQLite persistence    │
                                 └─────────────┬──────────┘
                                               │
                        ┌──────────────────────┼──────────────────────┐
                        │                      │                       │
              ┌─────────▼──────┐   ┌───────────▼──────┐   ┌──────────▼──────┐
              │ Traefik Ingress│   │  Prometheus/Grafana│   │  SQLite on PVC  │
              │ BasicAuth + TLS│   │  (metrics exposed) │   │  (5Gi RWO)      │
              └────────────────┘   └──────────────────┘   └────────────────┘
```

### Components

| Component | Path | Role |
|-----------|------|------|
| `edge/collector` | Go binary | Publishes simulated sensor JSON to MQTT every 5s |
| `mosquitto` | Eclipse Mosquitto 2.0 | MQTT message bus, TCP 1883 |
| `cloud/mqtt-bridge` | Go binary | Subscribes to MQTT, POSTs to ingestor HTTP API |
| `cloud/ingestor` | Go binary | REST API — validates + stores telemetry; serves /healthz, /readyz, /metrics |
| Traefik | k3s bundled | Ingress controller, BasicAuth middleware, TLS termination |
| SQLite | `modernc.org/sqlite` | Zero-cgo embedded storage, WAL mode, 5Gi PVC |

### Ports

| Service | Port | Protocol | Purpose |
|---------|------|----------|---------|
| mosquitto | 1883 | TCP/MQTT | MQTT broker |
| collector | 9090 | HTTP | Prometheus metrics |
| ingestor | 8080 | HTTP | REST API |
| ingestor | 9091 | HTTP | Prometheus metrics |
| Traefik | 80/443 | HTTP/HTTPS | Public ingress |
| k3s API | 6443 | HTTPS | Cluster API (via SSH tunnel only) |

---

## How to Demo in 30 Seconds

### One-time setup (first run only)

```bash
# 1. Install sshpass (macOS)
brew install hudochenkov/sshpass/sshpass

# 2. Set SSH password in env (never stored on disk)
export SSHPASS='<password>'

# 3. Run — this fetches kubeconfig automatically on first run
make demo
```

### Every subsequent run

```bash
export SSHPASS='<password>'
make demo
```

The script will:
1. Start an SSH tunnel to the k3s API (port 6443 is not public)
2. Set `KUBECONFIG` to `~/.kube/kubeconfig-iiot` (your default config is untouched)
3. Print nodes / pods / ingress / Traefik service
4. Prompt for the BasicAuth password (read interactively, never stored)
5. Run HTTP + HTTPS checks — expect `401` without auth, `200` with auth
6. POST a live telemetry reading through the ingress

Expected output (no secrets shown):

```
══════════════════════════════════════════════
  STEP 1 — SSH tunnel to k3s API
══════════════════════════════════════════════
[tunnel-up] already running (pid 12345) — nothing to do

  [PASS] GET /healthz   no-auth  → HTTP 401
  [PASS] GET /healthz   auth     → HTTP 200
  [PASS] GET /readyz    auth     → HTTP 200
  [PASS] POST /api/v1/telemetry  → HTTP 202  body: {"result":"accepted"}

══════════════════════════════════════════════
  DEMO OK — 7 checks passed, 0 failed
══════════════════════════════════════════════
```

To get the BasicAuth password (on the server only):

```bash
ssh root@<SERVER_IP> 'cat /opt/iiot/secrets/basic-auth-password.txt'
```

---

## Security Choices

### Why no real TLS certificate?

ACME/Let's Encrypt requires DNS validation (domain ownership). A bare IP address has no DNS delegation, so no public CA will sign a certificate for it. The demo uses a **self-signed cert with an IP SAN** — browsers warn, `curl -k` works, and it proves TLS termination is wired up correctly.

In production: add a domain → cert-manager + Let's Encrypt ClusterIssuer → automatic renewal.

### Why BasicAuth at the ingress?

It stops anonymous access before any traffic reaches application code. Traefik enforces it at the edge. For a demo this is proportionate and reversible. The password is bcrypt-hashed in the `iiot-basic-auth` Secret and never stored in git.

In production: replace with OIDC/OAuth2 (Traefik ForwardAuth + Keycloak/Dex).

### Why SSH tunnel for the Kubernetes API?

Port 6443 is **not** exposed publicly. The tunnel (`ssh -L 16443:127.0.0.1:6443`) forwards the API server through the encrypted SSH session only. This avoids the risk of a misconfigured firewall rule or exposed kubeconfig granting cluster access.

In production: VPN or bastion host with proper audit logging.

### Network isolation

All inter-service communication is ClusterIP (not NodePort). Only Traefik is reachable from the internet on ports 80/443 via the k3s `svclb` DaemonSet. The MQTT broker is cluster-internal.

---

## CI/CD

The release pipeline is tag-triggered:

```
git tag v1.2.3 && git push origin v1.2.3
         │
         └─► release.yml   — build + push images to GHCR
                 │
                 └─► deploy.yml  — kubectl set image + rollout status
```

### Release workflow

- Triggers on `v*` tags
- Builds `CGO_ENABLED=0` static Go binaries
- Produces distroless images (`gcr.io/distroless/static-debian12`)
- Pushes to GitHub Container Registry (`ghcr.io/<org>/iiot-edge-platform-golang/<service>:<tag>`)

### Deploy workflow

- Triggered by the same `v*` tag (runs after release)
- Requires `KUBECONFIG_BASE64` secret (base64-encoded kubeconfig with public IP substituted)
- Uses `kubectl set image` to update each deployment individually
- Waits for rollout via `kubectl rollout status --timeout`
- Probes `/healthz` and `/readyz` through a port-forward before marking success
- The `production` GitHub environment gate requires a reviewer to approve before deploying

### Rollback

```bash
# Roll back ingestor to the previous image
kubectl -n iiot rollout undo deployment/ingestor

# Or pin to a specific tag
kubectl -n iiot set image deployment/ingestor \
  ingestor=ghcr.io/<org>/iiot-edge-platform-golang/ingestor:v1.2.2
```

---

## Production vs Demo

| Concern | Demo (now) | Production |
|---------|-----------|------------|
| TLS | Self-signed, IP SAN | cert-manager + Let's Encrypt, domain |
| Auth | BasicAuth at ingress | OIDC/OAuth2 (Keycloak, Dex) |
| k8s API access | SSH tunnel | VPN or bastion + audit log |
| MQTT auth | Anonymous | TLS + password file or X.509 client certs |
| Storage | SQLite on emptyDir (ephemeral) / PVC (restart-durable) | TimescaleDB or InfluxDB on durable PV |
| Observability | Prometheus metrics exposed | Prometheus + Grafana + Alertmanager, PagerDuty |
| Ingestor replicas | 1 (RWO PVC, single writer) | Multi-replica with read replicas or distributed DB |
| Secret management | Manually generated on server | External Secrets Operator + Vault or cloud KMS |
| Rate limiting | None | Traefik `RateLimit` middleware |

---

## Troubleshooting

### SSH tunnel is down

```bash
# Restart it
export SSHPASS='<password>'
bash scripts/tunnel-up.sh

# Check status
bash scripts/tunnel-up.sh --status

# Inspect tunnel logs
cat /tmp/iiot-tunnel.log
```

### Wrong kubectl context

The demo scripts set `KUBECONFIG=~/.kube/kubeconfig-iiot` — they never touch your default
`~/.kube/config`. If you run kubectl in a new shell without sourcing `kube-env.sh`, it will
use your default context.

```bash
source scripts/kube-env.sh   # sets KUBECONFIG for current shell
kubectl -n iiot get pods
```

### Getting 404 from the ingress (not 401)

The Traefik Middleware may not have been applied yet, or the ingress annotation is wrong.

```bash
# Check middleware exists
KUBECONFIG=~/.kube/kubeconfig-iiot kubectl -n iiot get middleware

# Check ingress annotations
KUBECONFIG=~/.kube/kubeconfig-iiot kubectl -n iiot describe ingress iiot-ingress
```

### Pod is CrashLooping

```bash
KUBECONFIG=~/.kube/kubeconfig-iiot kubectl -n iiot logs \
  -l app.kubernetes.io/name=ingestor --tail=50 --previous
```

### Getting 401 when you expect 200

The password you entered doesn't match the one in the Kubernetes Secret.
The secret is synced from `/opt/iiot/secrets/users` (htpasswd file). If you regenerated
the password on the server without re-syncing the Secret:

```bash
# On the server
kubectl -n iiot create secret generic iiot-basic-auth \
  --from-file=users=/opt/iiot/secrets/users \
  --dry-run=client -o yaml | kubectl apply -f -
```

---

## Local Development

```bash
# Full k3d local cluster (no server needed)
make k3d-up          # create cluster + load images + deploy

# After code changes
make k3d-redeploy    # rebuild + re-deploy without recreating cluster

# Integration tests
make k3d-e2e         # full end-to-end: POST → GET → metrics
make k3d-e2e-db      # SQLite persistence + query endpoints

# Compose (no Kubernetes, fastest start)
make compose-up
curl http://localhost:8080/healthz  # → {"status":"ok"}
```

See [docs/runbook.md](docs/runbook.md) for the full local workflow reference.

---

## SRE Takeaways

- **Distroless images** (`CGO_ENABLED=0` + `gcr.io/distroless/static-debian12`) eliminate shell, package manager, and most CVE surface. The tradeoff is no `exec` debugging — use ephemeral debug containers.
- **Separate liveness from readiness.** `/healthz` tells Kubernetes the process is alive; `/readyz` tells it the DB is ready to serve traffic. Conflating the two causes premature traffic routing after a restart.
- **WAL mode for SQLite under Kubernetes.** Single-writer, multi-reader semantics with `_busy_timeout=5000` prevents `SQLITE_BUSY` panics on burst traffic without introducing a full RDBMS.
- **`sessionAffinity: ClientIP`** routes a client's follow-up reads to the same pod as its write — necessary with an in-process cache and no distributed store.
- **Idempotent manifests with `--dry-run=client | kubectl apply -f -`** make secret rotation safe to re-run from any state.
- **SSH tunnel beats firewall holes.** Never open 6443 publicly. The tunnel provides encryption, authentication, and auditability for free.
- **Tag-triggered CI/CD with a `production` environment gate** prevents accidental deploys while keeping automation intact for deliberate releases.
- **IP-SAN self-signed cert** is the correct engineering answer when no domain exists — not skipping TLS entirely, not using cert-manager without DNS.
