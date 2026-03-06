# Demo Checklist

Use this checklist before and during the demo to ensure everything is in order.
Complete each item in order — later steps depend on earlier ones.

---

## Pre-Demo Checklist

### 1. SSH Access

Verify you can reach the server before starting:

```bash
ssh -o ConnectTimeout=10 <SSH_USER_HERE>@<SERVER_IP_HERE> "echo ok"
```

Expected: `ok`

If this fails: check your network connection and verify the server is up.

---

### 2. Start SSH Tunnel

The Kubernetes API (port 6443) is not publicly exposed. Open the tunnel first:

```bash
bash scripts/tunnel-up.sh
```

Or check if it is already running:

```bash
bash scripts/tunnel-up.sh --status
```

Expected when running:

```
TUNNEL UP  — 127.0.0.1:16443 → <SERVER_IP_HERE>:6443  (pid XXXXX)
```

Leave this tunnel running for the duration of the demo.

---

### 3. Load kubeconfig

Set `KUBECONFIG` to the dedicated iiot kubeconfig (never touches `~/.kube/config`):

```bash
source scripts/kube-env.sh
```

Verify kubectl can reach the cluster:

```bash
kubectl cluster-info --request-timeout=8s
```

Expected:

```
Kubernetes control plane is running at https://127.0.0.1:16443
```

---

### 4. Verify Pods Are Running

```bash
kubectl get pods -n iiot
```

Expected — all four pods `1/1 Running`:

```
NAME                      READY   STATUS    RESTARTS   AGE
collector-<hash>          1/1     Running   0          Xd
ingestor-<hash>           1/1     Running   0          Xd
mosquitto-<hash>          1/1     Running   0          Xd
mqtt-bridge-<hash>        1/1     Running   0          Xd
```

If any pod is not `Running`: check logs with `kubectl logs <pod-name> -n iiot`.

---

### 5. Have the BasicAuth Password Ready

The password is stored on the server at `/opt/iiot/secrets/basic-auth-password.txt`.
Retrieve it before the demo:

```bash
ssh <SSH_USER_HERE>@<SERVER_IP_HERE> "cat /opt/iiot/secrets/basic-auth-password.txt"
```

Do not write the password down in any file that is tracked by git.

---

## Demo Commands

### Run the Automated Demo Script

```bash
make demo
```

This runs `scripts/demo-30s.sh` which:

1. Ensures the SSH tunnel is up (or starts it).
2. Loads the kubeconfig.
3. Prints a cluster / pod / ingress snapshot.
4. Prompts interactively for the BasicAuth password (never stored or echoed).
5. Runs HTTP and HTTPS curl tests (401 without auth, 200 with auth).
6. POSTs a live telemetry reading through the ingress.
7. Prints a pass/fail summary.

The password prompt appears at step 4. Type the password and press Enter.

---

### Manual Commands (if running step-by-step)

```bash
# Cluster snapshot
kubectl get nodes -o wide --request-timeout=8s
kubectl get pods -n iiot --request-timeout=8s
kubectl get ingress -n iiot --request-timeout=8s
kubectl get svc -n iiot --request-timeout=8s

# HTTP — expect 401 (no auth)
curl -s -o /dev/null -w "%{http_code}\n" http://<SERVER_IP_HERE>/healthz

# HTTP — expect 200 (with auth)
curl -s -u demo:*** http://<SERVER_IP_HERE>/healthz

# HTTPS — expect 401 (no auth, self-signed cert)
curl -sk -o /dev/null -w "%{http_code}\n" https://<SERVER_IP_HERE>/healthz

# HTTPS — expect 200 (with auth)
curl -sk -u demo:*** https://<SERVER_IP_HERE>/healthz

# POST telemetry — expect 202
curl -sk -u demo:*** \
  -X POST https://<SERVER_IP_HERE>/api/v1/telemetry \
  -H "Content-Type: application/json" \
  -d '{"device_id":"demo-device","timestamp":"2026-03-06T10:00:00Z","temperature_c":22.5,"pressure_hpa":1013.0,"humidity_pct":55.0,"status":"ok"}'
```

---

## Expected Behavior

| Step | What Happens |
|------|-------------|
| `make demo` starts | Tunnel script runs; prints "already running" if tunnel is up, or prompts for SSH password if not |
| Cluster snapshot | `kubectl` outputs nodes, pods, ingress — all over the local tunnel |
| Password prompt | `read -s` — terminal does not echo the typed password |
| `GET /healthz` no auth | HTTP 401 — Traefik BasicAuth middleware rejects the request |
| `GET /healthz` with auth | HTTP 200 — `{"status":"ok"}` from the ingestor |
| `GET /readyz` with auth | HTTP 200 — `{"status":"ok"}` (DB reachable) |
| HTTPS tests | Same results as HTTP but over TLS; `-k` skips cert verification |
| POST telemetry | HTTP 202 — `{"result":"accepted"}` — reading accepted and persisted to SQLite |
| Final summary | `DEMO OK — N checks passed, 0 failed` |

---

## Post-Demo Cleanup

### Kill the SSH Tunnel

```bash
# If started by tunnel-up.sh, the PID is tracked:
kill $(cat /tmp/iiot-tunnel.pid) 2>/dev/null && rm -f /tmp/iiot-tunnel.pid
echo "Tunnel stopped."
```

Or verify it is gone:

```bash
bash scripts/tunnel-up.sh --status
```

Expected: `TUNNEL DOWN`

### Unset kubeconfig (optional)

```bash
unset KUBECONFIG
```

---

## Troubleshooting

| Problem | Likely Cause | Fix |
|---------|-------------|-----|
| `kubectl: connection refused` | Tunnel not running | Run `bash scripts/tunnel-up.sh` |
| `kubectl: timeout` | Hanging kubectl, MaxStartups on sshd | Always pass `--request-timeout=8s` |
| `curl: 000` (no response) | Server unreachable or tunnel down | Check SSH tunnel status |
| `curl: 401` when authenticated | Wrong password | Re-read password from server |
| Pod not `Running` | Crash or image issue | `kubectl logs <pod> -n iiot` |
| Tunnel PID file stale | Previous crash | `rm /tmp/iiot-tunnel.pid` then re-run `tunnel-up.sh` |
