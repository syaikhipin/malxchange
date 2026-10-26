# malxchange

An open-source crypto exchange platform designed for **low-resource environments**. Run a full-featured spot exchange — matching engine, wallets, admin panel — on a single **4GB VPS** for ~€4/month.

![License](https://img.shields.io/badge/license-MIT-blue.svg)
![Go](https://img.shields.io/badge/go-1.21+-00ADD8.svg)
![Nuxt](https://img.shields.io/badge/nuxt-3-00DC82.svg)
![Postgres](https://img.shields.io/badge/postgres-17-336791.svg)

---

## Features

### Trading
![Trading](docs/screenshots/trading.png)
- **Real matching engine** — price-time priority, partial fills, market & limit orders
- **Live charts** — TradingView-powered candlestick charts with real-time updates
- **Order book** — live bid/ask depth
- **WebSocket feeds** — tickers, trades, order updates in real time

### Member Center
![Assets](docs/screenshots/trading-logged-in.png)
- **Assets** — portfolio overview with live balances
- **Deposit** — on-chain deposits with address generation (BTC, LTC)
- **Withdraw** — withdrawals with address book management
- **Order history** — current and historical orders with fill details

### Admin Panel
![Admin Dashboard](docs/screenshots/admin-dashboard.png)
- **Dashboard** — platform overview with granular permission chips (40+ permissions: member search, withdrawals, reconciliation, audit log, etc.)
- **Member management** — search by email/ID, view identity/status/KYC, freeze accounts (revokes live sessions)
- **KYC review** — compliance workflow for identity verification
- **Finance** — withdrawal approval queue, manual balance adjustments with audit trail
- **Operations** — matching engine status, ledger vs journal reconciliation (fold invariant), hot-wallet vs chain balance, incident tracking
- **Content** — announcements, help articles & categories
- **System** — employee accounts, roles & permissions
- **Audit log** — every admin action tracked
- **TOTP 2FA** — hardware-grade admin security (see setup below)

### Security
- Email verification for registration
- TOTP two-factor auth for admins
- Per-service Postgres roles (no cross-service DB access)
- Ledger/journal double-entry accounting with reconciliation

---

## Architecture

```
┌─────────────────────────────────────────┐
│              Single VPS                 │
│                                         │
│  ┌──────────┐  ┌──────────────────────┐ │
│  │  Caddy   │  │   5 Go microservices │ │
│  │ (TLS)    │  │   api, exchange,     │ │
│  └──────────┘  │   market, wallet,    │ │
│                │   admin-api          │ │
│                └──────────────────────┘ │
│  ┌──────────┐  ┌──────────┐ ┌────────┐  │
│  │ Postgres │  │ Redpanda │ │ Nuxt   │  │
│  │   17     │  │ (Kafka)  │ │ web +  │  │
│  └──────────┘  └──────────┘ │ admin  │  │
│                             └────────┘  │
└─────────────────────────────────────────┘
```

| Service | Description | RAM |
|---------|-------------|-----|
| `api` | Member REST API (auth, orders, wallets) | ~20 MB |
| `exchange` | Matching engine (order books, fills) | ~20 MB |
| `market` | Tickers, candles, market data | ~10 MB |
| `wallet` | Deposits, withdrawals, ledger | ~20 MB |
| `admin-api` | Admin REST API | ~10 MB |
| `web` | Member frontend (Nuxt 3) | ~75 MB |
| `admin` | Admin frontend (Nuxt SSR) | ~70 MB |
| `postgres` | PostgreSQL 17 (5 schemas) | ~250 MB |
| `redpanda` | Kafka-compatible event bus | ~400 MB |

**Total: ~1.1 GB** — fits comfortably on a 4GB VPS.

See [DEPLOYMENT_OPTIONS.md](DEPLOYMENT_OPTIONS.md) for split-service deployments (Vercel + managed DB/Kafka).

---

## Quick Start

### Prerequisites
- Docker + Docker Compose
- A VPS with 4GB+ RAM (or local machine for dev)
- A domain name (for production)

### 1. Clone
```bash
git clone https://github.com/syaikhipin/malxchange.git
cd malxchange
```

### 2. Configure
```bash
cp deploy/.env.example deploy/.env
# Edit deploy/.env — set passwords and domains
```

Required variables:
```bash
MAL_PG_SUPERUSER_PASSWORD=<strong-password>
MAL_PG_PASSWORD_API=<strong-password>
MAL_PG_PASSWORD_EXCHANGE=<strong-password>
MAL_PG_PASSWORD_MARKET=<strong-password>
MAL_PG_PASSWORD_WALLET=<strong-password>
MAL_PG_PASSWORD_ADMIN=<strong-password>
MAL_ADMIN_BOOTSTRAP_EMAIL=admin@yourdomain.com
```

### 3. Launch
```bash
cd deploy
docker compose -f docker-compose.yaml -f compose.demo-ltcd.yaml --profile chains up -d
```

This starts everything: Postgres, Redpanda, 5 Go services, both frontends, Caddy, and simnet chain nodes (ltcd/btcd for testing deposits).

### 4. Access
- **Member**: https://yourdomain.com
- **Admin**: https://admin.yourdomain.com

---

## Admin TOTP Setup

Admin accounts require TOTP two-factor authentication. Here's how to set it up:

### Step 1: Create the admin account
```bash
# The bootstrap admin is created on first run using:
# MAL_ADMIN_BOOTSTRAP_EMAIL from your .env
# Check logs for the temporary password:
docker logs malxchange-admin-api-1 2>&1 | grep -i "bootstrap\|password"
```

### Step 2: Log in (first time)
1. Go to `https://admin.yourdomain.com/login`
2. Enter the admin email and temporary password
3. You'll be prompted to set up TOTP

### Step 3: Scan the QR code
1. The admin panel will display a **QR code**
2. Open your authenticator app (Google Authenticator, Authy, 1Password, etc.)
3. Tap **+** → **Scan QR code**
4. Point your camera at the QR code

### Step 4: Verify
1. Enter the 6-digit code from your authenticator app
2. Click **Verify** — TOTP is now bound to your account
3. **Save your backup codes** in a safe place

### Step 5: Change your password
After first login, go to **Profile → Change Password** and set a strong password.

### Resetting TOTP (if you lose your device)
```bash
# On the VPS, clear the TOTP binding (requires DB access):
docker exec malxchange-postgres-1 psql -U malxchange -d malxchange \
  -c "UPDATE admin.employees SET totp_secret=NULL, totp_bound_at=NULL WHERE email='admin@yourdomain.com';"
# Then log in again — you'll be prompted to set up TOTP fresh
```

---

## API Reference

### Public endpoints
```
GET  /api/market/tickers          # All market tickers
GET  /api/market/depth/{pair}     # Order book depth
GET  /api/market/trades/{pair}    # Recent trades
GET  /api/market/candles/{pair}   # OHLC candles
```

### Authenticated endpoints
```
POST /api/uc/register/code        # Request email verification code
POST /api/uc/register             # Register with code
POST /api/uc/login                # Login (returns token)
GET  /api/uc/wallet/accounts      # Balances
POST /api/uc/orders               # Place order
GET  /api/uc/orders/current       # Open orders
GET  /api/uc/orders/history       # Order history
```

### WebSocket
```
wss://yourdomain.com/ws           # Live tickers, trades, order book
```

---

## Development

```bash
# Run Go tests
go test ./...

# Run frontend dev server
cd apps/web && npm run dev

# Run admin dev server
cd apps/admin && npm run dev
```

---

## Deployment Options

### Option A: All-in-One VPS (~€4/month)
Everything on a single Hetzner CX22 (4GB RAM). The full stack uses ~1.1GB:
- 5 Go microservices (~80MB total)
- PostgreSQL 17 (~250MB)
- Redpanda/Kafka (~400MB)
- Chain nodes ltcd/btcd (~170MB)
- Nuxt frontends (~150MB)

**Pros:** Cheapest, simplest, lowest latency. **Cons:** Self-managed backups, single point of failure.

### Option B: Split with Free Tiers (~€0-4/month)
- **Frontend → Vercel (free):** Deploy `apps/web` and `apps/admin` as separate Vercel projects. Set env vars:
  - `NUXT_PUBLIC_API_BASE_URL=https://your-api-domain/api`
  - `NUXT_PUBLIC_WS_URL=wss://your-api-domain/ws`
  - `NUXT_PUBLIC_ADMIN_API_BASE_URL=https://your-admin-api-domain/api`
- **PostgreSQL → Free tier:** Neon (free 512MB), Supabase (free 500MB), or Aiven free trial. Set `MAL_PG_HOST`, `MAL_PG_SSLMODE=require`.
- **Kafka → Free tier:** Upstash Kafka (free 10k msgs/day) or Aiven free trial. Or self-host Redpanda on the VPS.
- **Backend → VPS (€4/mo):** 5 Go services + Caddy + chain nodes only.

**Pros:** Managed DB backups, instant frontend deploys, preview URLs. **Cons:** Multi-vendor, higher latency to managed services.

See [DEPLOYMENT_OPTIONS.md](DEPLOYMENT_OPTIONS.md) for detailed setup guides.

---

## License

MIT — see [LICENSE](LICENSE) for details.

---

## Contributing

Pull requests welcome! Please:
1. Fork the repo
2. Create a feature branch
3. Add tests for new functionality
4. Submit a PR with a clear description
