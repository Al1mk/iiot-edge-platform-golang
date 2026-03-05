# Demo overlay: IP-only Traefik Ingress (no domain)

Owner/contact (for notes): alimk1752@gmail.com

## Goal
Expose the `ingestor` service via the server public IP only (no DNS, no host rules),
with Traefik Ingress + BasicAuth for demo security, and optional self-signed TLS.

## What this overlay contains
- `10-ingress.yaml`
  - Hostless Ingress: matches any Host header and works via `http(s)://<SERVER_IP>/`
  - References Traefik entrypoints `web` and `websecure`
  - Attaches BasicAuth middleware
  - Optionally enables TLS using secret `iiot-tls` (self-signed, IP SAN)

- `12-basicauth-middleware.yaml`
  - Traefik Middleware `iiot-basicauth` referencing secret `iiot-basic-auth`

- `kustomization.yaml`
  - Kustomize wiring for the demo overlay

## Prerequisites (on server)
Create BasicAuth secret (do not commit passwords):
- Generate htpasswd and store the password locally on server at:
  `/opt/iiot/secrets/basic-auth-password.txt` (chmod 600)
- Create secret in cluster:
  `kubectl -n iiot create secret generic iiot-basic-auth --from-file=users=/opt/iiot/secrets/auth --dry-run=client -o yaml | kubectl apply -f -`

Optional TLS:
- Create self-signed cert with IP SAN and create TLS secret `iiot-tls` in namespace `iiot`.

## How to apply
From your machine (or CI):
- `kubectl apply -k deploy/kustomize/overlays/demo`

## Verification
- Expect 401 without auth:
  `curl -i http://<SERVER_IP>/healthz`
- Expect 200 with auth:
  `curl -i -u demo:<password> http://<SERVER_IP>/healthz`

Optional HTTPS (self-signed):
- `curl -k -i https://<SERVER_IP>/readyz`

## Rollback
- Remove middleware and secrets:
  - `kubectl -n iiot delete middleware iiot-basicauth --ignore-not-found`
  - `kubectl -n iiot delete secret iiot-basic-auth --ignore-not-found`
  - `kubectl -n iiot delete secret iiot-tls --ignore-not-found`
- Re-apply a clean ingress without auth/TLS if desired.
