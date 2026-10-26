# malxchange

The spot-exchange core being rewritten in Go + Nuxt 3 from the legacy Java reference. This glossary is the shared domain language; terms here are the only correct names for these concepts in code, issues, and UI.

## Language

**Coin**:
A tradeable asset with a fixed decimal **scale** (BTC 8, ETH 18, USDT 6) and withdrawal/deposit mechanics.
_Avoid_: currency, token, asset

**Market**:
A tradeable pair of a base Coin against a quote Coin (e.g. BTC/USDT), carrying its tick size and lot size.
_Avoid_: symbol, pair, trading pair

**Tick size**:
The price quantum of a Market; order prices must be whole multiples of it.
_Avoid_: price step, precision

**Lot size**:
The amount quantum of a Market; order amounts must be whole multiples of it.
_Avoid_: step size, amount precision

**Scale**:
The number of decimal places a Coin is exact to; used for display, validation, and string serialization.
_Avoid_: decimals, precision

**Halted**:
A Market state where new Orders are rejected but existing Orders can still be cancelled.
_Avoid_: disabled, stopped

**Turnover**:
The quote-side value of a fill: price × amount.
_Avoid_: volume, notional

**Fill**:
The execution of some amount of an Order against another Order; produces fee and turnover.
_Avoid_: trade (a Trade is the market-wide record of a Fill pair), execution

**Maker**:
The Order resting in the book when a Fill happens; the side providing liquidity.
_Avoid_: passive order, quoter

**Taker**:
The Order that crosses the book and removes liquidity; every Fill has exactly one Maker and one Taker.
_Avoid_: aggressor, active order

**Fee**:
The charge a Fill levies on each side, taken in the Coin that side receives, at the Market's rate for that side's role.
_Avoid_: commission, realFee, discountFee

**Order**:
A member's instruction to buy or sell in a Market at a limit or market price.
_Avoid_: transaction, position

**Book**:
A Market's resting Orders in price-time priority, each with its remaining amount; rebuilt from open Orders whenever the engine starts.
_Avoid_: plate, ladder, order book

**Depth**:
The published per-price aggregation of one side of a Book.
_Avoid_: plate, order book

**Ticker**:
The rolling per-Market summary — last price, 24h range, turnover — published for all Markets on the ticker channel.
_Avoid_: thumb, CoinThumb, overview

**Kline**:
An OHLCV bar at a stored interval for a Market; the unit of chart history.
_Avoid_: candle, candlestick, OHLCV bar

**Warmup**:
The period after engine start during which a Market's Book is being rebuilt; commands wait and drain once the Market is Ready.
_Avoid_: boot, initialization (the rebuild act itself is _recovery_ per ADR-0010; Warmup names only the period it happens in)

**Ready**:
A Market state where the Book is rebuilt and commands are being consumed; engine-internal, with no admin toggle.
_Avoid_: enabled, started

**Member**:
An authenticated account that owns wallets, orders, and KYC state.
_Avoid_: user, customer, account

**Ledger**:
The single authoritative record of every Member's per-Coin balance (available and frozen), owned and written exclusively by the wallet service. Every fund movement appends a Journal entry.
_Avoid_: member_wallet, wallet (the service), balance table, custody (chain-side funds)

**Journal**:
The append-only record of every Ledger movement (deposit, withdrawal, fill settlement, fee, reward, adjustment) with its reason and idempotency key.
_Avoid_: member_transaction, transaction history, statement

**KYC**:
The Member identity-verification process and its states (unverified, pending, verified); verified KYC is required before withdrawals.
_Avoid_: real-name, identity verification, certification

**Verification code**:
A single-use, purpose-scoped short code with a TTL, delivered on a Code channel to prove control of a contact or confirm a sensitive action.
_Avoid_: OTP, SMS code, captcha

**Code channel**:
A transport that delivers Verification codes — email (always on) or an admin-activated messenger/SMS channel. Never a withdrawal factor.
_Avoid_: provider, gateway, SMS (as the generic term)

**Authenticator**:
A Member's TOTP binding; mandatory before the first withdrawal and required to confirm sensitive account changes.
_Avoid_: Google Auth, 2FA, googleState

**Deposit address**:
A chain address the exchange controls, allocated one per Member per Coin, that receives incoming funds.
_Avoid_: recharge address, wallet address

**Confirmation**:
The chain block depth accumulated by a deposit; each Coin sets the depth — its confirmation count — at which the deposit credits the Ledger.
_Avoid_: block maturity, verification

**Hot wallet**:
The exchange-controlled wallet whose keys live encrypted on the host; it signs withdrawals and receives sweeps.
_Avoid_: withdraw wallet, operational wallet

**Cold wallet**:
The exchange-controlled wallet whose keys are kept offline; it holds funds beyond the hot wallet's operating needs.
_Avoid_: offline wallet, storage wallet

**Sweep**:
An exchange-initiated move of chain-side funds: from Member deposit addresses to the hot wallet, or excess hot funds to the cold wallet.
_Avoid_: collect, consolidation

**Employee**:
A staff account that authenticates to the admin app; identified by email, holding exactly one Role. Employees administer Members, Markets, Coins, and custody — they are never Members.
_Avoid_: admin (as a person), staff, user

**Role**:
The single Role an Employee holds; a named bundle of Permissions (super-admin, finance, compliance, support) with editable grants over the seeded catalog.
_Avoid_: department, group, user type

**Permission**:
A capability string (verb:resource) that exactly one admin endpoint checks; seeded from the endpoint list and read-only everywhere, granted to Roles.
_Avoid_: menu item, privilege, access right

**Step-up**:
A fresh TOTP code demanded at action time for fund-moving or credential-changing admin actions, beyond the session's login factor.
_Avoid_: re-auth, second password, 2FA prompt

**Audit log**:
The append-only record of every admin mutation and authentication event — actor, action, target, before/after, origin IP — written by middleware, never by individual handlers.
_Avoid_: access log, operation log, admin log

**Order intent**:
The api-side record of a member's placement, gate-inserted before the order command exists; the idempotety seam that makes duplicate submissions and producer races safe.
_Avoid_: pending order, pre-order

**Favorites**:
A member's pinned Markets, kept per member and served to the watchlist on the trading page.
_Avoid_: follows, starred markets

**Withdrawal address book**:
The member-managed list of saved withdrawal destinations per Coin.
_Avoid_: whitelist, payee list

**Session**:
An authenticated device's opaque token row: member sessions slide seven days from last use; Employee sessions cap at twelve hours absolute plus thirty minutes idle, and revocation kills the row, not the cookie.
_Avoid_: JWT, login state

**Keystore**:
The encrypted hot-wallet key file (argon2id + XChaCha20-Poly1305), created offline by walletctl during the key ceremony and decrypted only inside the signer's memory.
_Avoid_: wallet file, key file

**Break-glass**:
The host-side CLI that recovers a locked-out sole super-admin; its actions are audited as a system actor inside the same transaction as the change.
_Avoid_: backdoor, emergency access

**Incident**:
A Wallet Ops record of a custody anomaly — a reorg eviction, a failed broadcast, a deep-reorg invalidation — awaiting operator adjudication.
_Avoid_: alert, ticket
