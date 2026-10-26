# malxchange Whole-System Deep Audit — 2026-10-26

**Scope:** full-stack — architecture, matching-engine and Ledger correctness, custody, security, API, frontends, code quality, tests, CI, deployment, performance. Audited tree: `main @ dda0dc0` (verified identical to `syaikhipin/malxchange` and `sulthonzh/malxchange` at audit time).

**Method:** seven parallel domain audits (engine/events, wallet/custody, auth/admin, data/CI/deploy, frontends, convention-drift, performance), every finding traced to `file:line` evidence and spot-verified by the coordinating reviewer; plus container-executed baselines on an isolated engine (go vet/build/unit, both Nuxt suites, production builds, `go test -race`, `go test -cover`, `govulncheck`, `npm audit`, CI-parity integration suite with a race pass). No file in the audited tree was modified.

**Verdict:** the core is genuinely well-engineered — deterministic engine with transactional-batch persistence, a single-writer Ledger with constraint-backed idempotency, custody that keeps only watch-only keys on the host, an unusually disciplined review-checklist culture, zero TODO debt, 12 of 13 ADRs verified compliant, and zero data races anywhere the race detector ran. The system's weaknesses cluster in four places: **round-trip amplification on the money path** (3 critical), **failure-path semantics in custody and eventing** (order-of-event handling after the happy path), **the documentation-to-reality seam** (README ships a stack shape the compose file does not contain), and **the absence of enforced budgets** (RAM, connections, abuse rates, CI security). None of these require re-architecture; every improvement in the roadmap is code-or-config-only inside the founding constraint: **high performance, lightweight, single 4GB VPS**.

**Counts:** 3 critical · 24 high · 45 medium · 50 low · 6 reachable dependency CVEs — consolidated from seven domain audits (190+ raw observations; cross-domain duplicates merged, e.g. the TLS fail-open appears in both the wallet and deploy audits). Full per-finding detail with evidence ships in `2026-10-26-issues.md` alongside this report; actionable findings are grouped there as ready-to-file issue bodies.

---

## 1. System architecture as verified

Five Go services (`api`, `exchange`, `market`, `wallet`, `admin-api`) plus two Nuxt 3 SSR apps, Postgres 17 (five schemas, one owner role per service, zero cross-schema FKs, cross-service reads proven blocked by the integration suite), Redpanda v25.1.12 with SASL SCRAM, Caddy at the edge, simnet btcd/ltcd + anvil + tronstub under test profiles.

- **Trading:** `api` validates a placement against per-Market config, synchronously freezes the exact hold over the wallet's internal RPC, and enqueues an `order.command` envelope through its transactional outbox. The `exchange` service folds commands through per-Market in-memory Books (int64 ticks/lots per ADR-0005, price-time priority), persisting each batch — order rows, Fill rows, outbox events — in one Postgres transaction, committing Kafka offsets only after it (ADR-0010). Recovery rebuilds Books from open Orders in sequence order and drains.
- **Settlement:** `wallet` consumes `trade.fill` through its inbox and posts six Journal lines per Fill (per side: frozen debit, gross credit, explicit fee in the received Coin) in one transaction, idempotent twice over (inbox primary key plus Journal idem keys). The Ledger is single-writer with `CHECK (x >= 0)` balance columns and the open-at-zero movement pattern from the review checklist.
- **Custody:** deposit addresses are HD-derived from watch-only xpubs, one per Member per Coin; confirmations BTC 3 / LTC 6 / EVM 12 / TRON 20; the hot wallet keystore is argon2id + XChaCha20-Poly1305, decrypted only in the signer's memory with a derived-vs-recorded address cross-check at boot; sweeps are threshold-based and pausable; cold keys never touch the host (ADR-0008, verified, with one letter-level divergence noted in 4.6).
- **Member plane:** email-only identity, argon2id, opaque session tokens carried in a header (no cookie, no CSRF surface), sliding 7-day expiry, single-use purpose-scoped verification codes; withdrawals require KYC-verified plus a bound Authenticator plus a live TOTP code, checked server-side before any wallet call.
- **Employee plane:** httpOnly-cookie sessions (12 h absolute + 30 min idle), mandatory TOTP, per-request permission middleware over a code-seeded read-only catalog, step-up TOTP on all eight ADR-0012 action classes, append-only audit log enforced by a database trigger. Zero IDOR and zero SQL-injection findings; bootstrap and break-glass both verified one-shot/audited.

This matches the ADR set faithfully. The findings below are where implementation and documentation diverge from that intent.

---

## 2. Critical findings (money path performance)

All three are round-trip amplification on the hot path; all three are fixable with `pgx` features already vendored — no schema changes, no new dependencies.

| # | Finding | Evidence | Effect |
|---|---------|----------|--------|
| C1 | **Fill settlement costs ~23 sequential DB round-trips per Fill, all inside one transaction.** `Post` alone is three statements (Journal insert, open-at-zero insert, balance update); each side of a Fill is hold-debit + hold settle (update + guard select) + gross credit + optional fee. | `internal/wallet/ledger.go:67-108`, `settlement.go:47-84`, `holds.go:63-86`; batch of up to 500 in `consumer.go:25,58-71` | Settlement ceiling on the order of 100–200 Fills/s (statement count measured; timing estimated). The hold settle collapses to one `UPDATE ... RETURNING`; the whole side pipelines. |
| C2 | **The engine's persistence transaction serializes per-Fill and per-command statements one at a time.** ~4 statements per Fill, single-row inbox inserts in a loop, ~1,500–2,500 round-trips for a 500-record batch. | `internal/exchange/apply.go:36-48`, `engine.go:207-218`, `consumer.go:25` | Real throughput sits roughly an order of magnitude below ADR-0010's 50-command/5 ms model. `pgx.Batch` collapses this to a few flushes with identical semantics. |
| C3 | **Every placement runs a `SELECT count(*)` inside the batch transaction and inside the global Book mutex** — and the check is silently skipped when the query errors (`err == nil && open >= cap`). | `internal/exchange/engine.go:59-61,164-171` | A per-placement round-trip under the engine-wide lock, where the engine already owns the data to answer in memory in O(1). The error-swallow is also a correctness defect (the cap silently stops being enforced on a DB hiccup). |

---

## 3. High findings

### 3.1 Engine and eventing
- **H1 — Phantom liquidity: a fully-consumed Book side can never clear the published Depth.** The engine returns `Items: nil` for an empty side (`internal/engine/engine.go:542-563`), the wire type omits empty sides (`omitempty`, `internal/events/orders.go:75-76`), and the projector treats an absent side as "keep last-known" (`internal/market/projection.go:209-229`). A market order that sweeps every bid leaves stale bid Depth served on REST and WebSocket indefinitely — the market service's own test admits the producer cannot emit the shape (`consumer_it_test.go:602-607`). Five-hop chain verified end-to-end.
- **H2 — Outbox per-key ordering is claimed but not delivered.** The relay's own comment promises same-key enqueue order (`internal/outbox/outbox.go:195-197`), but a failed publish backs off while later same-key rows publish immediately (order inversion on retry, `outbox.go:219-235`), and same-transaction rows share one `created_at`, which the unstable sort cannot order (`outbox.go:198`). No test pins ordering.

### 3.2 Wallet and custody
- **H3 — A zero-amount ERC-20/TRC-20 Transfer permanently wedges that Coin's deposit watcher.** Token transfer parsing has no positivity check (`evm.go:228-244`, `tron.go:154-166`); the insert violates `deposits CHECK (amount > 0)`; `Tick` errors before the cursor saves, so the same block re-scans forever, halting credits for every Member of that Coin. Zero-value token transfers are a cheap, address-targetable spam vector.
- **H4 — The UTXO spend index is write-once, and reconciliation audits it against itself.** `spendUtxo` only marks spent; a reorg that evicts a spending transaction permanently corrupts the index (`watcher.go:286-301`), and for BTC/LTC the "chain side" of reconciliation sums the same `utxos` table the audit is checking (`run.go:216-229`) — structurally blind to its own corruption.
- **H5 — Signed-state crash recovery nulls the recorded chain txid and strands the withdrawal.** On re-broadcast failure the recovery path calls `MarkWithdrawalBroadcast(ctx, db, w.ID, "")` whose `NULLIF` destroys the only record of a possibly-broadcast transaction, then targets a state the row no longer has (`executor.go:109-118`, `withdrawals.go:220-222`) — stuck in `broadcast` with no txid, unconfirmable forever.
- **H6 — An evicted broadcast withdrawal/sweep/hot-transfer has no reachable failed state.** `failEvicted` only files an incident; `MarkWithdrawalFailed` is pinned to `approved→failed`; refund and retry both require `failed` (`executor.go:237-285`, `withdrawals.go:228-230`). A Member stays debited with no admin action short of manual SQL.
- **H7 — Chain TLS verification fails open by default.** Unset `MAL_CHAIN_TLS_SKIP_VERIFY` means skip-verify (`wallet/config.go:58`), and the production compose hardcodes it true (`docker-compose.yaml:224`) — unverified TLS on the RPC that drives deposit crediting, carrying btcd basic-auth.
- **H8 — One poison event wedges all settlement, and a panic bypasses the fatal log.** Any `SettleEnvelope` error is fatal by design; one malformed `trade.fill` blocks every market's settlement in a permanent restart loop (`wallet/consumer.go:41-71`), and `money.MustParse` panics before the `run.go:70-73` fatal-cause log the #51 checklist rule promises (`settlement.go:31-35`). Same disease as the exchange-side duplicate-ID crash loop (`apply.go:97-121`, `consumer.go:61-78`) — the system has no dead-letter surface anywhere.

### 3.3 Data, CI, deployment
- **H9 — `replace-me` placeholder credentials can silently reach production.** Compose interpolates with `:-` defaults and zero `:?` required-variable guards (`docker-compose.yaml:49-57` and throughout); `internal/config/config.go:95-99` rejects only empty strings, so the literal placeholder boots as the real Postgres superuser password. Only the installer mints real secrets, and the README never mentions it.
- **H10 — The documented production entry story is broken.** README's quick start references `deploy/.env.example` (actual file: `deploy/env.example`) and `compose.demo-ltcd.yaml` (does not exist) — `README.md:101,119`, `DEPLOYMENT_OPTIONS.md:65,142`. A README-follower cannot complete setup without improvising.
- **H11 — Chain nodes default to mainnet with `--txindex` on a stack documented for a 40 GB VPS.** `docker-compose.yaml:321-347` vs `DEPLOYMENT_OPTIONS.md:9`; the installer's own preflight warns "chains alone want 100GB+" (`install.sh:58`). The default install fills the recommended disk.
- **H12 — The production edge pins Caddy's internal CA.** `tls internal` on both vhosts (`Caddyfile:8,26`) — untrusted-certificate warnings on a money platform; no ACME path exists.
- **H13 — Backups are same-host, unencrypted, unalerted, and never restore-tested** — while carrying KYC document images and plaintext TOTP secrets in the dump (`deploy/pg-backup/backup.sh:9-26`, `api/0006_security.up.sql:18-19`, `admin/0002_auth.up.sql:35`). The weakest link for a Ledger-bearing system.
- **H14 — CI has no security scanning of any kind.** No trivy/gosec/govulncheck anywhere in `.github/` or the Makefile; `govulncheck` run for this audit found 6 reachable CVEs (see 5.3) that nothing would have caught.
- **H15 — The demo seeder is production-reachable and silently broken.** It seeds bcrypt hashes the argon2id-only verifier rejects (login/order legs no-op, exit 0 — `seed-demo.sh:21-24` vs `internal/auth/password.go:80-81`), while its direct-SQL legs still land: `'credited'` deposits with no Journal entries and Ledger balances echoed only by self-referential adjustments (`seed-demo.sh:38-52`) — a Ledger-vs-Journal fold break on any database it touches, with no environment guard. `seed-dummy.sql` ships a known-password super-admin row and contradicts ADR-0012's rejected-options list.

### 3.4 Documentation vs reality
- **H16 — The member web frontend is not in the production stack, but README ships it as if it were.** The `web` service is a commented-out block ("container lands with issue #24", `docker-compose.yaml:356-359`) while the Caddyfile still proxies `web:3000` (`Caddyfile:21-22`) and README's architecture diagram, RAM table, and launch instructions include it. The documented quick start yields a member domain that 502s.
- **H17 — No LICENSE file exists** while README claims MIT and links one (`README.md:5,282-284`). An "open-source" repository with no license text.

### 3.5 Frontends
- **H18 — Admin param pages fetch `/api/members/undefined`.** `members/[id].vue:104-106` and `finance/withdrawals/[id].vue:106-108` read route params via `defineProps` without `definePageMeta({ props: true })` — the exact failure class the review checklist documents. The web app's filesystem guard spec has no admin twin, and no admin e2e journey opens either page.
- **H19 — The trading page never re-resolves when the market changes.** `loadInitial()` is the only place `route.params.pair` is read and it runs only in `onMounted` (`[pair].vue:601-618`); with no `watch` and no `:page-key` (`app.vue:3`), clicking another Market changes the URL but keeps the old Market's Book, chart, orders, and balances.
- **H20 — The member e2e suite never runs in CI.** `make member-e2e` exists (`Makefile:47`) with the repo's best frontend journeys (WebSocket authz, decimal exactness, lockout); ci.yml runs only vitest+build for web.

### 3.6 Data-path performance
- **H21 — The Kline fold costs 16 round-trips per Fill** (select+upsert × 8 intervals, including selects on fresh buckets, inside the projection transaction — `internal/market/kline.go:202-259`).
- **H22 — Depth persistence costs up to ~200 statements per engine batch** (one delete plus one insert per level per side — `internal/market/projection.go:208-231`).
- **H23 — The ticker pass runs a per-second full-`trades` scan plus a per-Market 24-hour read into Go, and `/market/tickers` is a per-Market N+1** — on a table that is never pruned (`internal/market/ticker.go:86-138,229-253`, `rpc.go:359-389`).
- **H24 — The member Journal endpoint has no LIMIT on an append-only table, while api caps the proxy read at 1 MB** — as Journals grow, the records page degrades to a 503 (`internal/wallet/rpc.go:581-625`, `internal/api/funds.go:114`).

---

## 4. Medium and low findings (condensed)

The clusters below list the notable items — not an exhaustive census. Individually drafted medium findings carry issue bodies in `2026-10-26-issues.md` (ISSUE-28 through ISSUE-41); the remaining medium and low items are consolidated into the grouped issues (ISSUE-42 through ISSUE-47).

**Correctness / money safety (9 medium):** duplicate order-ID command is a permanent engine crash loop (`apply.go:97-121`); quantization freeze-guard race silently truncates on recovery (`recover.go:87-104` — reproduces the legacy truncation ADR-0005 rejected); self-trade unhandled at every layer while the WebSocket hub drops the sell-side personal Fill for self-trades (`hub.go:293`); fill-against-unheld-order settles silently when the Member has other frozen funds (`holds.go:63-86` — the checklist's exact bug class); reorg window is 2×confirmations so deeper reorgs evicting credited deposits go undetected (ADR-0008 divergence); recon's in-flight term only counts `broadcast`, so the gate reads not-OK in normal operation (`reconcile.go:82`); every fresh broadcast files a false "evicted" incident (mempool depth −1 conflated with eviction — alarm fatigue); sweep fee pre-funding moves real TRX with no recorded txid; `min_amount`/`max_amount` compared against a market-buy's quote budget (dimensional mismatch).

**Security (4 medium):** no global/per-IP rate limiting anywhere — distributed password spraying is invisible and code-issuance can pump unlimited distinct targets (email bombing; SMS toll fraud once a channel activates); TOTP codes are never consumed — replay works within the ±1-step window on login, step-up, and the withdrawal gate; a committed balance adjustment drops its audit row when the after-snapshot read fails (`audit.go:89-96` — contradicts ADR-0012's gapless promise); the internal RPC plane is unauthenticated behind a single network boundary (ADR-accepted, but any compromised container is total funds control).

**Data/ops (8 medium):** `sslmode=disable` hardcoded and `MAL_KAFKA_SEEDS` not interpolated — the documented split-deploy Option B is unreachable as shipped; no compose resource limits or log caps (the 1.1 GB budget is enforced by nothing); dead knobs (`MAL_PEPPER` generated but never read, `MAL_WALLET_NET/POLL` never injected); CI hygiene (no permissions block, no concurrency, no timeouts); installer verification accepts `exited` as healthy; role credentials are once-per-volume; KYC images in-row; chain nodes run as root.

**Frontend (8 medium):** member session cookie is JS-readable without `secure: true`; `SecurityCodeButton` emits success on failure; no 401 recovery on either app; member identity never restored after refresh; favorites race; zero client-side tick/lot/scale/balance validation; deposit amounts hardcoded to scale 8; shared error slot renders chart failures inside order forms; resize-listener leak.

**Drift/quality (9 medium + others):** Go badge 1.21+ vs go.mod 1.26; nonexistent `candles` route documented (actual: `kline`); review-checklist claims a `make lint` readyz enforcement that does not exist; `Symbol` and `Plate` identifier families carry no canonical waiver markers; `seed-dummy.sql` dead and ADR-contradicting; ADR-0007 partial (web container, bootstrap scope); waiver-format violations; one `decimal.New(3, -3)` at the checklist's own documented mistake-site (`evm/e2e_test.go:279`); duplicated `MarketInfo` type already drifted in web.

**Low (36):** notable ones — `assign:employee-role` (super-admin promotion) demands no step-up; XFF first-hop trust; SMTP opportunistic cleartext fallback; per-email lockout as a DoS lever; no security headers at the edge; inbox tables never pruned (unbounded growth on the money path); `internal/wallet` has zero untagged unit tests (money core tested only through the integration suite); anvil `:latest` floating tag; admin container has no healthcheck; fills table lacks positivity CHECKs and order-id indexes; `t.Skip` debt (btc e2e, issue #28); zero git tags/releases; glossary gaps (Session, Order intent, Withdrawal address book, Break-glass, Incident, Favorites, Keystore undefined); a handful of dead code and debug logs.

---

## 5. Test, CI, and supply-chain health

### 5.1 Baselines (container-verified, isolated engine)
- Go: `go vet` clean, `go build ./...` clean, unit suite **17/17 packages green**.
- apps/web: **49/49** tests; apps/admin: **149/149** tests. Both production builds succeed (4.79 MB / 5.19 MB; client bundles 1.3 MB / 1.2 MB — import discipline is good).
- Race detector: **zero data races** across every unit package and the wallet subtree.
- Integration suite (CI parity, `-tags integration -p 1`, 18 packages): **all green** — including the heavyweights: exchange 187 s, market 200 s, wallet 172 s, admin 43 s, api 33 s.
- Integration suite under `-race`: **all 18 packages green, zero data races** — the real concurrency paths (consumer loops, outbox relay, watchers, WebSocket hub) are race-clean, corroborating the unit-level race pass.

### 5.2 Coverage shape (unit-level; integration suites carry the rest)
Engine 89.8%, money 88.0%, config 88.9%, events 84.2% — the core money logic is well unit-tested. Service shells lean on integration suites: exchange 15.8%, api 16.5%, admin 7.2%, outbox 11.3%, and `internal/wallet` 0% untagged. Untested paths that matter: TTL sweeper (zero coverage anywhere), outbox relay/ordering, all reorg behavior, executor crash-recovery states, reconciliation formula, duplicate-ID poison, self-trade.

### 5.3 Supply chain
`govulncheck`: **6 reachable vulnerabilities in 3 modules** — `golang.org/x/net v0.58.0` (GO-2026-6617 h2 crash, -6612 flow-control refund, -6611 CPU exhaustion, -6603 memory exhaustion; fixed v0.60.0), `google.golang.org/grpc v1.83.1` (GO-2026-6443 server panic; fixed v1.83.2), `github.com/gorilla/websocket v1.5.0` indirect (GO-2026-6278 weak PRNG; fixed v1.5.3) — with call traces through the EVM chain client, the TRON stub, and the admin bootstrap RPC. `npm audit`: **0 vulnerabilities in both apps**. All fixes are dependency bumps.

### 5.4 CI
Seven jobs (lint+guards+unit, integration, web, admin, build-images, test-profile with a SASL negative test, admin-e2e) with zero silent-pass constructs and unusually strong repo-specific lint guards. Missing: security scanning, member-e2e, workflow permissions/concurrency/timeouts, image publishing, any release/versioning (zero tags exist).

---

## 6. Performance and weight assessment

The founding budget — ~1.1 GB RAM on a 4 GB VPS — is currently **aspirational, not enforced**: no container has a memory limit, Redpanda is capped at 1 GB internally (vs ~400 MB documented), no `GOMEMLIMIT`/`GOMAXPROCS`/automaxprocs anywhere, pool sizes are `max(4, NumCPU)` per service with no env knob against Postgres's default 100 connections, and the chains profile (`--txindex`) can consume multi-GB by itself. The good news: the audit found no architectural weight problems — topic retentions are bounded, buffers are capped, there are no goroutine or connection leaks, indexes match issued queries, no transaction is held across a network call, and both frontend bundles are lean. The throughput ceilings are all round-trip amplification (section 2), fixable with batching already available in `pgx`. The 20-item improvement table from the performance audit is entirely code-or-config: no new containers, no heavyweight dependencies, no schema migrations.

---

## 7. Improvement roadmap — "extremely better" under the lightweight constraint

Every item states impact, weight cost, and effort. Acceptance criteria for each live in the issue draft. Ordering within a tier is by risk-adjusted value.

### Tier 0 — Correctness and trust (days; mostly small diffs)
1. **Dependency bumps for the 6 CVEs** (x/net 0.60.0, grpc 1.83.2, gorilla/websocket 1.5.3). Impact: closes every reachable CVE. Cost: zero. Effort: S.
2. **Poison-tolerance at the money boundaries:** reject non-positive token deposit amounts in the EVM/TRON adapters (kills the deposit-watcher wedge); per-envelope attempts cap plus park/dead-letter in both consumers; `recover()` around settlement so a panic cannot bypass the fatal log. Impact: deposits and settlement survive malformed input. Cost: zero. Effort: M.
3. **Custody state-machine honesty:** never null a recorded txid (attempt-history column); wire `broadcast→failed` for evicted withdrawals/sweeps/hot-transfers so refund/retry are reachable; distinguish mempool-unknown from evicted depth. Impact: no more stranded-fund states requiring manual SQL. Cost: one column + careful transitions. Effort: M.
4. **Fail-closed transport + placeholder guards:** default `MAL_CHAIN_TLS_SKIP_VERIFY` to verify-on (skip only under the simnet profile); add `:?` interpolation guards for every secret in compose (or a startup placeholder check). Impact: removes two silent foot-guns. Cost: zero. Effort: S.
5. **TOTP step consumption** (persist `last_used_step` per binding; ~20 lines + one migration). Impact: closes the replay window on withdrawals and step-up. Cost: one column. Effort: S.
6. **Empty-side Depth fix** (drop `omitempty` on Plate sides; projector already supports present-but-empty as clear). Impact: phantom liquidity dies on REST and WS. Cost: zero. Effort: S.
7. **Frontend hot-fixes:** `props: true` + spec port for the two admin param pages; `watch`/`:page-key` on the exchange page plus a Playwright leg that switches markets; `secure: true` on the member session cookie; stop emitting `sent` on failure. Impact: two user-visible breakages fixed; token exposure reduced. Cost: zero. Effort: S.
8. **Truth-sweep the entry story:** add LICENSE; land the `web` container in compose or correct README's stack shape; fix the nonexistent file/compose references, the Go badge, the `candles` route, and the mainnet/disk claims; fix the installer's `exited*` acceptance. Impact: the documented path works. Cost: zero. Effort: S.
9. **Seeder:** fix (API-driven or a tiny Go seeder using `auth.HashPassword`) or delete both seed scripts; add an environment guard. Impact: removes a Ledger-invariant breaker from the docs. Cost: zero. Effort: S.
10. **CI teeth:** member-e2e job; govulncheck + gosec + trivy (fs and image) failing on high; permissions/concurrency/timeout blocks. Impact: the classes this audit found become CI-caught. Cost: CI minutes only. Effort: S.
11. **Small correctness patches:** `MaxOpen` error path + in-memory count (also removes C3's round-trip); audit-row `Force` when an adjustment committed; Journal endpoint LIMIT + aligned proxy cap; remove debug logs and dead code. Effort: S each.

### Tier 1 — Throughput ceilings (1–2 weeks; the criticals and data-path highs)
12. **`pgx.Batch` the settlement and engine persistence transactions** (multi-row inbox inserts, merged accrual updates, batched outbox inserts, `UPDATE ... RETURNING` hold settle). Impact: ~4–6× fewer round-trips on the money path; the single highest-leverage change in the system. Cost: zero (vendored feature). Effort: M.
13. **Kline fold to a SQL upsert** (one statement per interval, or one `unnest` for all eight; same determinism keys). Impact: 16→1–2 round-trips per Fill. Cost: zero. Effort: M.
14. **Ticker to one aggregate query** (`GROUP BY market` over a `(market, traded_at)` index) + fix the `/tickers` N+1. Impact: per-second load drops from O(all 24 h Fills decoded in Go) to one indexed scan. Cost: zero. Effort: M.
15. **Depth persist via `unnest`** (2 statements per batch). Impact: ~200→4 round-trips. Cost: zero. Effort: S.
16. **Outbox ordering + relay efficiency:** monotonic claim key, defer later same-key rows on failure, batch the mark updates, gate reclaim to poll ticks; pin with the ordering test the checklist mandates. Impact: per-key ordering becomes real; relay round-trips halve. Cost: one column or clock-timestamp default. Effort: M.
17. **Constant-factor sweep:** cache parsed fee rates on MarketCfg; cache `pow10` and add an int64 fast path in the match loop; fanout early-return on zero subscribers; per-coin `WatchAddresses`; construct-time `Decimals` check; `money.Parse` without the regex pre-check. Impact: removes per-Fill and per-tick waste across engine, market, api, wallet. Cost: zero. Effort: S each.

### Tier 2 — Hardening and enforceable budgets (weeks)
18. **UTXO/custody index repair on reorg + independent verification:** re-derive `utxos` from the re-scanned window on rewind; source the BTC/LTC "chain side" from the node rather than the audited table; add `audit/approved/signed` as explicit recon components; schedule the fold check. Impact: reconciliation becomes a tripwire that can actually fire true. Cost: zero new infra. Effort: M-L.
19. **Internal-plane authentication:** one static shared bearer token (compose-injected) on every `/internal/` request via the boot harness — converts "any container compromise = total funds control" into a two-failure problem. Cost: one env var. Effort: S-M.
20. **Backup and PII hardening:** off-host push, encryption at rest, failure alerting, a rehearsed restore runbook (ideally an IT that restores into a throwaway database); envelope-encrypt TOTP secrets, channel credentials, and KYC images with a key outside the backup path. Impact: a stolen backup stops being a live 2FA factor and identity-document breach. Effort: M.
21. **Enforced budgets:** compose `mem_limit` per container sized to the ADR-0007 table; `GOMEMLIMIT` on Go services; `MAL_PG_MAX_CONNS` wired and defaulted to a total under Postgres's budget; optionally `uber.org/automaxprocs` (tiny). Impact: the 4 GB ceiling becomes real. Cost: config only. Effort: S.
22. **Abuse controls:** per-IP/global rate limiting at the edge or api middleware for login/register/forgot/code; an aggregate per-account daily cap on code issuance; step-up on role assignment and grant edits; `OriginPatterns` pinning; security headers (HSTS/CSP/XCTO) in Caddy. Effort: M.
23. **Test debt where it hurts:** reorg suite (window, invalidate, spend-index under rewind), executor crash-recovery states, TTL sweeper, outbox relay, poison paths, reconciliation formula, self-trade policy tests, fill-against-unheld; lift `internal/wallet` unit coverage off zero. Effort: M.

### Tier 3 — Strategic (decisions, then work)
24. **Self-trade policy ADR** (allow/prevent, at which layer) + the WebSocket sell-Fill suppression fix; **market-order resting visibility** (invisible resting orders hold frozen funds with no Depth representation — either document or publish); **dust-burn sign-off** (current behavior charges the buyer's remainder — in tension with ADR-0005's "dust stays with the member"). These are product decisions the code currently makes by omission.
25. **SSR strategy:** either use the SSR being paid for (`useAsyncData` on public pages) or stop paying for it (routeRules prerender/SWR; the two Node containers exist because ADR-0007 assumed SSR data fetching that never happened). Also self-host fonts (render-blocking third party today).
26. **Release discipline:** tags + image publishing + a changelog; today every deploy is "rebuild main".
27. **`research/legacy-schema` keep-or-prune**; canonicalize the three waiver markers; add the readyz lint the checklist promises (or correct the checklist); close the glossary gaps (Session, Order intent, Withdrawal address book, Break-glass, Incident, Favorites, Keystore).

**What excellent looks like from here:** the same five services and two apps, still inside 1.1 GB, but with batched money-path persistence (an order-of-magnitude throughput headroom), poison-tolerant consumers with dead-letter surfaces, custody state machines that cannot strand funds, a reconciliation tripwire that runs on a schedule and compares against the chain rather than itself, backups that survive the loss of the host, CI that catches CVEs and runs every e2e suite, and documentation that matches the stack it describes. Nothing on this list adds a container or a heavyweight dependency.

---

## Appendix A — Evidence index
- Baseline outputs (vet/build/unit/race/cover/vuln/npm-audit/builds/IT) reproduced in the audit session log; key numbers in section 5.
- Per-finding evidence with file:line for every item above: `docs/audits/2026-10-26-issues.md` (ready-to-file issue bodies, grouped, with red-first acceptance criteria per `docs/agents/issue-tracker.md`).
- ADR compliance: 12/13 clean (0001–0006, 0008–0013), 0007 partial (web container absent; installer bootstrap scope drifted to migrations).
- Glossary compliance of this document: terms follow `GLOSSARY.md` (Market, Book, Depth, Fill, Ledger, Journal, Member, Employee, Authenticator, Step-up).
