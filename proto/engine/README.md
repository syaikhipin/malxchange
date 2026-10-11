# PROTOTYPE — Go matching-engine core (throwaway, never merged)

Answers the map ticket [Prototype the Go matching-engine core](https://github.com/syaikhipin/malxchange/issues/10):
**do the ported CoinTrader.java semantics feel right in Go, on an int64 tick/lot book?**

Port source: `00_framework/exchange/.../Trader/CoinTrader.java` (+ `TradePlate`, `ExchangeOrderConsumer`).
Glossary terms apply: Fill (not "trade"), Order, Market, tick size, lot size.

## Run

```
go run ./cmd/demo       # drivable playground → http://localhost:8090
go run ./cmd/roundtrip  # Kafka protocol round-trip via kfake + franz-go, verified vs direct run
go run ./cmd/bench      # orders/sec, tick-to-trade latency, cancels, depth snapshots
```

## What is ported (legacy semantics, kept deliberately)

- Price-time priority: price-sorted level lists (bids desc, asks asc), FIFO within a level.
- Limit taker: crosses the opposite limit book (bounded by its price), then eats resting
  market orders **at the taker's own price**, remainder rests on the book.
- Market orders never rest in the depth plate. Market **buy amount = quote budget**;
  market sell amount = base quantity (legacy `ExchangeOrder.isCompleted`).
- Dust burn: a market buy whose leftover budget buys zero additional lots at the current
  price is completed and the leftover is folded into that fill's buy turnover
  (legacy `adjustMarketOrderTurnover`).
- Fill price = the resting maker's limit price (price improvement stays with the taker).
- Depth plate: per-price aggregation of remaining amounts, max 100 levels, market orders
  excluded (legacy `TradePlate`).
- Trading halt refuses new orders (the api service auto-cancels them in production).

## Deliberate divergences (decisions for the real engine ticket)

1. **Cancel by order ID only** — legacy needed side+type+price hints; the Go book keeps an
   ID index.
2. **Depth published once per batch per side**, not on every mutation.
3. **FENTAN pro-rated publish mode not ported** (IEO-style gimmick; propose cutting).
4. **Fill economics on the cold path use math/big**: turnover = priceTicks × amountLots ×
   TickRaw × LotRaw / 10^BaseScale, and affordable-lots division, overflow int64 at
   realistic scales (e.g. 5,001,000 × 10,000 × lots). Hot book path stays pure int64 —
   that split is the ADR-0005 caveat made concrete.

## Findings feed the engine spec ticket

See the resolution comment on the ticket for the verdict and numbers.
