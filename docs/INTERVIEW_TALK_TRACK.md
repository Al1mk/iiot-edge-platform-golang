# Interview Talk Track — IIoT Edge Platform

> Contact: alimk1752@gmail.com

---

## 1-Minute Narrative

"This is a production-shaped IIoT data pipeline running on a single-node k3s cluster on a Hetzner VPS. It has no domain — just an IP — so I had to solve a few interesting engineering problems to make it demo-ready and portfolio-ready.

The pipeline has four Go services: a collector that simulates field sensor readings and publishes them over MQTT, a broker (Eclipse Mosquitto), a bridge that subscribes and forwards to HTTP, and an ingestor that validates the telemetry schema, persists it to SQLite, and exposes Prometheus metrics.

On the infrastructure side: Traefik handles ingress with a hostless rule — no `spec.rules.host` — so it responds to a bare IP. I layered BasicAuth at the Traefik middleware level and generated a self-signed cert with an IP SAN for HTTPS. Since there's no public DNS, Let's Encrypt is off the table.

For CI/CD I have a tag-triggered workflow: `git tag v1.x.x` → GitHub Actions builds distroless images, pushes to GHCR, then a deploy workflow does `kubectl set image` + rollout status + health probe, all behind a production environment gate so I don't deploy accidentally.

The Kubernetes API is never exposed publicly. I access it exclusively through an SSH tunnel forwarding local port 16443 to the server's loopback 6443 — encrypted, authenticated, and revocable.

The whole thing demos in under 30 seconds with one command: `make demo`."

---

## 5 Likely Interview Questions

### Q1: Why SSH tunnel instead of exposing port 6443?

The API server port (6443) on a single-node k3s cluster runs with the cluster admin certificate. Exposing it publicly means anyone who finds that port can attempt to authenticate against it. The attack surface is the kubeconfig itself.

An SSH tunnel gives you encryption, authentication, and auditing for free via the existing SSH daemon. It's also completely revocable — kill the tunnel, kill the access. No firewall rule changes, no cloud security group edits.

In production you'd use a VPN (WireGuard, Tailscale) or a bastion host with audit logging. The SSH tunnel is the correct solution at this scale and cost.

### Q2: Why self-signed TLS instead of cert-manager?

ACME (Let's Encrypt) requires proof of domain ownership — either HTTP-01 (serve a challenge file at `/.well-known/acme-challenge/`) or DNS-01 (add a TXT record). Neither works with a bare IP.

cert-manager is installed on the cluster (I saw it running), but it can't issue a certificate for `91.107.174.94` without a domain in front of it. Self-signed with an IP SAN is the technically correct answer — it proves the TLS termination path is wired up correctly. The browser warning is expected and explained to the audience.

### Q3: Why Traefik instead of Nginx Ingress?

Traefik ships bundled with k3s. That's the main reason — zero additional installation. It supports CRD-based middleware natively (`traefik.io/v1alpha1 Middleware`), which made adding BasicAuth a single manifest instead of a separate controller or Nginx `auth_basic` config reload.

Traefik also has built-in dashboard, automatic Let's Encrypt support (when DNS is available), and a clean routing model with priorities. For a production cluster I'd evaluate either, but for a demo on k3s, Traefik is the natural choice.

### Q4: How does the CI/CD pipeline work end to end?

1. Developer pushes a `v*` tag (e.g. `v1.2.3`)
2. `release.yml` triggers: builds three Go binaries with `CGO_ENABLED=0`, packages them into distroless images, pushes to GHCR tagged with the semver version
3. `deploy.yml` triggers on the same tag: fetches the kubeconfig from `KUBECONFIG_BASE64` secret (base64 of k3s.yaml with server URL set to the public IP), runs `kubectl set image` for each deployment, waits for `kubectl rollout status`, then port-forwards and probes `/healthz` and `/readyz`
4. The deploy job runs inside a `production` GitHub environment — a reviewer must approve before kubectl touches the cluster

The two workflows are decoupled: `release.yml` produces artifacts, `deploy.yml` consumes them. This means you could re-trigger deploy with a different image tag without rebuilding.

### Q5: How does rollback work if a deploy goes wrong?

```bash
# Immediate rollback to last good revision
kubectl -n iiot rollout undo deployment/ingestor

# Or pin to a specific image tag
kubectl -n iiot set image deployment/ingestor \
  ingestor=ghcr.io/<org>/iiot-edge-platform-golang/ingestor:v1.2.2

# Wait for it
kubectl -n iiot rollout status deployment/ingestor --timeout=120s
```

Kubernetes keeps the previous ReplicaSet around by default (`revisionHistoryLimit: 10`), so rollback is instant — it just scales the old RS back up. The `/readyz` probe prevents traffic from routing to the pod until SQLite is confirmed reachable. The ingestor uses `strategy: Recreate` because the PVC is `ReadWriteOnce` and can't be mounted by two pods simultaneously — so the old pod terminates before the new one starts.

---

## 30-Second Incident Response Story

**Scenario:** Alert fires — ingestor 5xx rate spike. Pods look Running but readyz is 503.

**My first 30 seconds:**

```bash
# 1. Is it the DB?
kubectl -n iiot exec -it deploy/ingestor -- curl -s localhost:8080/readyz
# → {"status":"error","error":"db ping failed"}

# 2. DB metrics
kubectl -n iiot port-forward svc/ingestor 19091:9091 &
curl -s localhost:19091/metrics | grep iiot_db
# → iiot_db_up 0   ← DB is down

# 3. PVC status
kubectl -n iiot get pvc telemetry-db
# → STATUS: Bound  — PVC is fine

# 4. SQLite file
kubectl -n iiot exec -it deploy/ingestor -- ls -lh /var/data/iiot.db
# → permission denied  ← securityContext issue after restart

# 5. Root cause: pod restarted as uid 65532, but PVC was written by previous uid
kubectl -n iiot describe pod <pod> | grep -A5 securityContext
# Confirm fsGroup not set — PVC ownership wasn't inherited
```

**Fix:** add `fsGroup: 65532` to the pod `securityContext` so the volume is chowned on mount.

```yaml
spec:
  securityContext:
    runAsNonRoot: true
    runAsUser:    65532
    runAsGroup:   65532
    fsGroup:      65532   # ← this was missing
```

**Rollout:** `kubectl apply`, wait for rollout status, confirm `iiot_db_up 1` in metrics.

**The lesson:** `fsGroup` is easy to miss when running distroless with a non-root UID and a PVC. The readiness probe caught it before users noticed — that's exactly what readiness probes are for.

---

## Quick Reference — What's Where

| Thing | Location |
|-------|----------|
| Ingress (demo) | `deploy/kustomize/overlays/demo/10-ingress.yaml` |
| BasicAuth middleware | `deploy/kustomize/overlays/demo/12-basicauth-middleware.yaml` |
| Deploy CI/CD | `.github/workflows/deploy.yml` |
| Release CI/CD | `.github/workflows/release.yml` (if present) |
| Demo scripts | `scripts/demo-30s.sh`, `scripts/tunnel-up.sh`, `scripts/kube-env.sh` |
| Architecture doc | `docs/architecture.md` |
| Full runbook | `docs/runbook.md` |
| Demo config | `scripts/demo.env` (no secrets) |
