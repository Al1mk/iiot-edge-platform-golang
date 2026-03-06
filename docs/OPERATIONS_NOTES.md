# Operations Notes

## 1. Cluster Overview

Single-node k3s cluster hosted on a VPS running Ubuntu 24.04.4 LTS.

| Property | Value |
|----------|-------|
| Distribution | k3s v1.34.4+k3s1 |
| Node role | control-plane (single node) |
| Ingress controller | Traefik (bundled with k3s) |
| Datastore | kine SQLite (embedded, default k3s mode) |
| Namespace | `iiot` |
| Public access | IP-only (no domain), ports 80 + 443 |

kubectl access is not available directly over the public internet. Port 6443 (k3s API
server) is not exposed. All kubectl operations require an SSH tunnel:

```
local 127.0.0.1:16443 → remote 127.0.0.1:6443
```

See `scripts/tunnel-up.sh` and `scripts/kube-env.sh` for the tunnel and kubeconfig setup.

---

## 2. Architecture Summary

```
Internet
    │
    ▼
[Traefik Ingress]  port 80 / 443  (IP-only, BasicAuth, self-signed TLS)
    │
    ├── /healthz, /readyz, /api/v1/telemetry  →  ingestor:8080
    │
    └── (MQTT internal only)
           mosquitto:1883
               ▲           ▼
          collector     mqtt-bridge  →  ingestor:8080
```

- **collector** — publishes simulated sensor readings to Mosquitto via MQTT every 5 s.
- **mosquitto** — Eclipse Mosquitto MQTT broker; internal to the cluster (ClusterIP only).
- **mqtt-bridge** — subscribes to `iiot/telemetry`, forwards each reading to the ingestor via HTTP POST.
- **ingestor** — validates and persists telemetry to SQLite; exposes REST API and Prometheus metrics.

---

## 3. Namespace Layout

All workloads run in the `iiot` namespace.

```
kubectl get all -n iiot
```

Expected resource types:

| Kind | Names |
|------|-------|
| Deployment | collector, ingestor, mosquitto, mqtt-bridge |
| Pod | one pod per deployment (replicas: 1) |
| Service | collector (ClusterIP), ingestor (ClusterIP), mosquitto (ClusterIP), mqtt-bridge (ClusterIP) |
| Ingress | iiot-ingress (Traefik, hosts: *, ports 80+443) |
| Secret | iiot-basicauth (htpasswd for Traefik BasicAuth middleware) |
| Secret | iiot-tls (self-signed TLS cert with IP SAN) |
| Middleware | iiot-basicauth (Traefik CRD, traefik.io/v1alpha1) |

---

## 4. Pod Verification

```bash
kubectl get pods -n iiot
```

Expected output — all pods `1/1 Running`, zero restarts under normal operation:

```
NAME                           READY   STATUS    RESTARTS   AGE
collector-<hash>               1/1     Running   0          Xm
ingestor-<hash>                1/1     Running   0          Xm
mosquitto-<hash>               1/1     Running   0          Xm
mqtt-bridge-<hash>             1/1     Running   0          Xm
```

If a pod is in `CrashLoopBackOff` or `Pending`:

```bash
kubectl describe pod <pod-name> -n iiot
kubectl logs <pod-name> -n iiot
```

---

## 5. Ingress Verification

```bash
kubectl get ingress -n iiot
```

Expected output:

```
NAME           CLASS     HOSTS   ADDRESS           PORTS     AGE
iiot-ingress   traefik   *       <SERVER_IP_HERE>  80, 443   Xd
```

`HOSTS: *` is correct — this is an IP-only ingress with no domain name.

```bash
kubectl get svc -n iiot
```

Expected — all services are ClusterIP (no NodePort or LoadBalancer needed; Traefik
handles external traffic):

```
NAME          TYPE        CLUSTER-IP      PORT(S)             AGE
collector     ClusterIP   10.x.x.x        9090/TCP            Xd
ingestor      ClusterIP   10.x.x.x        8080/TCP,9091/TCP   Xd
mosquitto     ClusterIP   10.x.x.x        1883/TCP            Xd
mqtt-bridge   ClusterIP   10.x.x.x        9092/TCP            Xd
```

Traefik itself is exposed as a LoadBalancer service in `kube-system`:

```bash
kubectl get svc traefik -n kube-system
```

---

## 6. Security Design

| Layer | Mechanism |
|-------|-----------|
| HTTP authentication | Traefik BasicAuth middleware (`iiot-basicauth`) — 401 without credentials |
| Transport encryption | Self-signed TLS certificate with IP SAN `<SERVER_IP_HERE>`; use `curl -k` |
| API server access | Port 6443 not public; SSH tunnel required |
| Secrets | BasicAuth htpasswd stored in Kubernetes Secret `iiot-basicauth` |
| TLS secret | `iiot-tls` in namespace `iiot`; issued offline, no cert-manager dependency |

The Traefik middleware annotation on the Ingress:

```
traefik.ingress.kubernetes.io/router.middlewares: iiot-iiot-basicauth@kubernetescrd
```

Format: `<namespace>-<middleware-name>@kubernetescrd`

The Traefik CRD group is `traefik.io/v1alpha1` (k3s 1.34+). Older references to
`traefik.containo.us` are incorrect for this cluster version.

---

## 7. cert-manager Cleanup Verification

cert-manager was removed from this cluster (no public domain means ACME is not viable;
the self-signed cert is managed manually). Verify it is fully gone:

```bash
# Namespace should not exist
kubectl get ns | grep cert-manager

# No cert-manager CRDs should be present
kubectl get crd | grep cert-manager

# No issuers or certificates should exist cluster-wide
kubectl get clusterissuer,issuer,certificate -A
```

Expected output for all three commands: **empty / no resources found**.

If any cert-manager resources remain, they will continuously attempt ACME challenges and
generate noisy API server write traffic, which causes kine SQLite WAL growth. Remove any
remnants before declaring the cluster clean.
