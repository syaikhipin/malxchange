# malxchange Deployment Options

Two supported deployment architectures for the malxchange crypto exchange.

---

## Option A: All-in-One VPS (~€4/month)

Everything runs on a single Hetzner VPS (CX22: 2 vCPU, 4GB RAM, 40GB disk).

### Architecture

```
┌─────────────────────────────────────────┐
│              Hetzner VPS (~€4/mo)        │
│                                         │
│  ┌──────────┐  ┌──────────────────────┐ │
│  │  Caddy   │  │   5 Go microservices │ │
│  │ (TLS +   │  │   api, exchange,     │ │
│  │  proxy)  │  │   market, wallet,    │ │
│  └──────────┘  │   admin-api          │ │
│                 └──────────────────────┘ │
│  ┌──────────┐  ┌──────────┐ ┌────────┐  │
│  │ Postgres │  │ Redpanda │ │ Nuxt   │  │
│  │   17     │  │ (Kafka)  │ │ web +  │  │
│  └──────────┘  └──────────┘ │ admin  │  │
│                             └────────┘  │
│  ┌──────────┐  ┌──────────┐             │
│  │  ltcd    │  │  btcd    │             │
│  │ (mainnet)│  │ (mainnet)│             │
│  └──────────┘  └──────────┘             │
└─────────────────────────────────────────┘
```

### Resource usage (measured)

| Component            | RAM    |
|--------------------|--------|
| 5 Go microservices | ~80 MB |
| Postgres 17        | ~250 MB|
| Redpanda           | ~400 MB typical (capped at 1 GB)|
| ltcd + btcd        | ~170 MB|
| Nuxt web + admin   | ~150 MB|
| Caddy + misc       | ~50 MB |
| **Total**          | **~1.1 GB** |

Comfortably fits in 4GB with headroom for spikes.

### Pros
- **Cheapest**: ~€4/month total
- **Simplest**: one box, one docker-compose, no cross-service networking
- **Lowest latency**: everything on localhost
- **Full control**: no vendor lock-in

### Cons
- **You manage everything**: Postgres backups, Kafka ops, TLS certs
- **Single point of failure**: VPS goes down = everything goes down
- **Manual frontend deploys**: rebuild Docker images for every frontend change
- **No preview deployments** for frontend changes

### Setup
```bash
# On the VPS
cd /root/malxchange/deploy
docker compose -f docker-compose.yaml up -d        # add --profile chains for in-compose nodes
```

---

## Option B: Split Services (~€11/month)

Current production setup: frontend on Vercel, data on Aiven, backend on VPS.

### Architecture

```
┌──────────────────┐     ┌──────────────────┐
│  Vercel (free)   │     │  Aiven (~€7/mo)  │
│                  │     │                  │
│  malxchange-web  │     │  Postgres        │
│  (Nuxt member)   │     │  (managed)       │
│                  │     │                  │
│  malxchange-     │     │  Kafka           │
│  admin           │     │  (managed)       │
│  (Nuxt admin)    │     │                  │
└────────┬─────────┘     └────────┬─────────┘
         │ HTTPS/WSS              │ TLS
         ▼                        ▼
┌─────────────────────────────────────────┐
│           Hetzner VPS (~€4/mo)          │
│                                         │
│  ┌──────────┐  ┌──────────────────────┐ │
│  │  Caddy   │  │   5 Go microservices │ │
│  │ (TLS +   │  │   api, exchange,     │ │
│  │  proxy)  │  │   market, wallet,    │ │
│  └──────────┘  │   admin-api          │ │
│                └──────────────────────┘ │
│  ┌──────────┐  ┌──────────┐             │
│  │  ltcd    │  │  btcd    │             │
│  │ (mainnet)│  │ (mainnet)│             │
│  └──────────┘  └──────────┘             │
└─────────────────────────────────────────┘
```

### Cost breakdown

| Service              | Cost       |
|----------------------|------------|
| Vercel (2 projects)  | €0 (free tier) |
| Aiven Postgres       | ~€3.50/mo  |
| Aiven Kafka          | ~€3.50/mo  |
| Hetzner VPS (CX22)   | ~€4/mo     |
| **Total**            | **~€11/mo**|

### Pros
- **Managed data layer**: automated Postgres backups, Kafka ops handled by Aiven
- **Instant frontend deploys**: push to `main` → Vercel builds in ~2 min
- **Preview deployments**: every PR gets its own URL
- **Better separation**: frontend/backend/data scale independently

### Cons
- **More expensive**: ~€11/month vs ~€4/month
- **More moving parts**: Vercel + Aiven + VPS = 3 vendors
- **Slightly higher latency**: VPS ↔ Aiven network hop for DB/Kafka
- **More config**: CORS, env vars, TLS certs across services

### Setup
```bash
# 1. Vercel: connect syaikhipin/malxchange repo
#    - malxchange-web → root: apps/web
#    - malxchange-admin → root: apps/admin
#    Env vars (already set):
#    - NUXT_PUBLIC_API_BASE_URL=https://exchange.arifin.uk/api
#    - NUXT_PUBLIC_WS_URL=wss://exchange.arifin.uk/ws
#    - NUXT_PUBLIC_ADMIN_API_BASE_URL=https://admin.exchange.arifin.uk/api

# 2. Aiven: create Postgres + Kafka services, note connection strings

# 3. On the VPS: update deploy/.env with Aiven credentials
#    and docker-compose.yaml with Aiven hosts, then:
cd /root/malxchange/deploy
docker compose -f docker-compose.yaml up -d
```

---

## Recommendation

| Scenario | Pick |
|----------|------|
| Grading/demo, minimize cost | **Option A** (~€4/mo) |
| Active development, frequent frontend changes | **Option B** (~€11/mo) |
| Production with real users | **Option B** (managed backups worth it) |

For the current grading/demo phase, **Option A** is sufficient and saves ~€7/month.
