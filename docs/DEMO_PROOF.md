# Demo Proof

This document shows the expected output for each verification step of the live demo.
All commands are run from the Mac after the SSH tunnel is established and the kubeconfig
is loaded (see `DEMO_CHECKLIST.md`).

---

## 1. Pod Status

```bash
kubectl get pods -n iiot
```

Expected output:

```
NAME                           READY   STATUS    RESTARTS   AGE
collector-<hash>               1/1     Running   0          Xd
ingestor-<hash>                1/1     Running   0          Xd
mosquitto-<hash>               1/1     Running   0          Xd
mqtt-bridge-<hash>             1/1     Running   0          Xd
```

All four pods must be `1/1 Running` with zero restarts.

---

## 2. Ingress and Services

```bash
kubectl get ingress -n iiot
```

Expected output:

```
NAME           CLASS     HOSTS   ADDRESS              PORTS     AGE
iiot-ingress   traefik   *       <SERVER_IP_HERE>     80, 443   Xd
```

```bash
kubectl get svc -n iiot
```

Expected output:

```
NAME          TYPE        CLUSTER-IP      PORT(S)             AGE
collector     ClusterIP   10.x.x.x        9090/TCP            Xd
ingestor      ClusterIP   10.x.x.x        8080/TCP,9091/TCP   Xd
mosquitto     ClusterIP   10.x.x.x        1883/TCP            Xd
mqtt-bridge   ClusterIP   10.x.x.x        9092/TCP            Xd
```

---

## 3. HTTP Tests (port 80)

### Unauthenticated — must return 401

```bash
curl -s -o /dev/null -w "%{http_code}" http://<SERVER_IP_HERE>/healthz
```

Expected:

```
401
```

```bash
curl -s http://<SERVER_IP_HERE>/healthz
```

Expected body:

```
401 Unauthorized
```

### Authenticated — must return 200

```bash
curl -s -u demo:*** http://<SERVER_IP_HERE>/healthz
```

Expected:

```
{"status":"ok"}
```

```bash
curl -s -u demo:*** http://<SERVER_IP_HERE>/readyz
```

Expected:

```
{"status":"ok"}
```

---

## 4. HTTPS Tests (port 443, self-signed cert)

The TLS certificate is self-signed with an IP SAN. Use `-k` to skip certificate
verification in curl (expected for self-signed; a browser will show a warning).

### Unauthenticated — must return 401

```bash
curl -sk -o /dev/null -w "%{http_code}" https://<SERVER_IP_HERE>/healthz
```

Expected:

```
401
```

### Authenticated — must return 200

```bash
curl -sk -u demo:*** https://<SERVER_IP_HERE>/healthz
```

Expected:

```
{"status":"ok"}
```

```bash
curl -sk -u demo:*** https://<SERVER_IP_HERE>/readyz
```

Expected:

```
{"status":"ok"}
```

---

## 5. Telemetry POST Through Ingress

```bash
curl -sk -u demo:*** \
  -X POST https://<SERVER_IP_HERE>/api/v1/telemetry \
  -H "Content-Type: application/json" \
  -d '{
    "device_id":     "demo-device",
    "timestamp":     "2026-03-06T10:00:00Z",
    "temperature_c": 22.5,
    "pressure_hpa":  1013.0,
    "humidity_pct":  55.0,
    "status":        "ok"
  }'
```

Expected:

```json
{"result":"accepted"}
```

HTTP status: `202 Accepted`

---

## 6. Validation Rejection

```bash
curl -sk -u demo:*** \
  -X POST https://<SERVER_IP_HERE>/api/v1/telemetry \
  -H "Content-Type: application/json" \
  -d '{"device_id":"","timestamp":"2026-03-06T10:00:00Z","temperature_c":22.5,"pressure_hpa":1013.0,"humidity_pct":55.0,"status":"ok"}'
```

Expected:

```json
{"error":"validation_failed","message":"device_id is required"}
```

HTTP status: `422 Unprocessable Entity`

---

## 7. Automated Demo Script Output

Running `make demo` (or `bash scripts/demo-30s.sh`) produces output similar to:

```
══════════════════════════════════════════════
  STEP 1 — SSH tunnel to k3s API
══════════════════════════════════════════════
[tunnel-up] already running (pid 12345) — nothing to do

══════════════════════════════════════════════
  STEP 2 — Kubernetes access
══════════════════════════════════════════════
  kubectl → Kubernetes control plane is running at https://127.0.0.1:16443

══════════════════════════════════════════════
  STEP 3 — Cluster snapshot
══════════════════════════════════════════════

  [nodes]
    NAME        STATUS   ROLES                  AGE   VERSION
    <hostname>  Ready    control-plane,master   Xd    v1.34.4+k3s1

  [pods — namespace: iiot]
    NAME                      READY   STATUS    RESTARTS   AGE
    collector-<hash>          1/1     Running   0          Xd
    ingestor-<hash>           1/1     Running   0          Xd
    mosquitto-<hash>          1/1     Running   0          Xd
    mqtt-bridge-<hash>        1/1     Running   0          Xd

  [ingress]
    NAME           CLASS     HOSTS   ADDRESS              PORTS     AGE
    iiot-ingress   traefik   *       <SERVER_IP_HERE>     80, 443   Xd

══════════════════════════════════════════════
  STEP 4 — HTTP / HTTPS demo checks
══════════════════════════════════════════════

  --- HTTP (port 80) ---
  [PASS] GET /healthz   no-auth  → HTTP 401
  [PASS] GET /readyz    no-auth  → HTTP 401
  [PASS] GET /healthz   auth     → HTTP 200
  [PASS] GET /readyz    auth     → HTTP 200

  --- HTTPS (port 443, self-signed cert — curl -k) ---
  [PASS] GET /healthz   no-auth  → HTTP 401
  [PASS] GET /healthz   auth     → HTTP 200
  [PASS] GET /readyz    auth     → HTTP 200

  --- POST telemetry (through ingress, authenticated) ---
  [PASS] POST /api/v1/telemetry → HTTP 202  body: {"result":"accepted"}

══════════════════════════════════════════════
  DEMO OK — 8 checks passed, 0 failed
══════════════════════════════════════════════
```

All checks must show `[PASS]` and the final line must read `DEMO OK`.
