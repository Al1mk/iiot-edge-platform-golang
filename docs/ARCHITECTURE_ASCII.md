# System Architecture (ASCII)

This diagram represents the internal architecture of the IIoT telemetry pipeline
running on a single-node k3s cluster.

```
┌─────────────────────────────────────────────────────────────┐
│                        VPS  /  k3s                          │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐  │
│  │                   namespace: iiot                    │  │
│  │                                                      │  │
│  │   Collector                                          │  │
│  │      │  MQTT                                        │  │
│  │      ▼                                              │  │
│  │   Mosquitto                                         │  │
│  │      │  subscribe                                   │  │
│  │      ▼                                              │  │
│  │   MQTT Bridge                                       │  │
│  │      │  HTTP POST                                   │  │
│  │      ▼                                              │  │
│  │   Ingestor API ──── SQLite                          │  │
│  │      ▲                                              │  │
│  │      │ HTTP                                         │  │
│  │   Traefik Ingress                                   │  │
│  │   BasicAuth · TLS                                   │  │
│  └──────────────────────────────────────────────────┬─┘  │
│                                                      │     │
│                                         k3s API :6443│     │
└──────────────────────────────────────────────────────┼─────┘
                         │                             │
              HTTPS :443 │              SSH tunnel     │
                         │          localhost:16443    │
               ┌─────────┴──────┐   ┌─────────────────┴──┐
               │  User/Recruiter│   │       Admin         │
               └────────────────┘   └────────────────────┘
```
