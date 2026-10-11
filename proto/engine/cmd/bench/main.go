// THROWAWAY PROTOTYPE (branch proto/engine). Laptop benchmark for the ported
// book: sustained orders/sec, per-command (tick-to-trade) latency percentiles,
// cancel throughput, and depth-snapshot cost.
package main

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"time"

	engine "github.com/syaikhipin/malxchange/proto/engine/engine"
)

var spec = engine.MarketSpec{
	Symbol: "BTC/USDT", BaseScale: 8, QuoteScale: 2,
	TickRaw: 1, LotRaw: 10000,
}

const mid = 50000_00 // 50,000.00 USDT in ticks

func pct(lat []time.Duration, p float64) time.Duration {
	if len(lat) == 0 {
		return 0
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat[int(float64(len(lat)-1)*p)]
}

func main() {
	rng := rand.New(rand.NewPCG(42, 42))

	// ---- seed: 500 levels per side × 3 orders each ----
	book := engine.NewBook(spec)
	var seedCmds []engine.Command
	n := 0
	for i := range 500 {
		for range 3 {
			seedCmds = append(seedCmds, engine.Command{Place: &engine.PlaceIn{
				ID: fmt.Sprintf("S%d", n), Member: "maker", Side: engine.Sell, Kind: engine.Limit,
				Price: spec.PriceString(mid + 10 + int64(i)), Amount: spec.AmountString(int64(1 + n%5)),
			}})
			n++
			seedCmds = append(seedCmds, engine.Command{Place: &engine.PlaceIn{
				ID: fmt.Sprintf("S%d", n), Member: "maker", Side: engine.Buy, Kind: engine.Limit,
				Price: spec.PriceString(mid - 10 - int64(i)), Amount: spec.AmountString(int64(1 + n%5)),
			}})
			n++
		}
	}
	t0 := time.Now()
	book.Batch(seedCmds)
	fmt.Printf("seed      : %6d maker orders booked in %s\n", len(seedCmds), time.Since(t0).Round(time.Microsecond))

	// ---- phase 1: sustained mixed limit flow (churn + crossings) ----
	const N1 = 200_000
	var fills, completed int
	t0 = time.Now()
	for i := range N1 {
		lvl := int64(rng.IntN(41) - 20) // ±20 ticks around mid
		side := engine.Buy
		if i%2 == 1 {
			side = engine.Sell
		}
		evs := book.Batch([]engine.Command{{Place: &engine.PlaceIn{
			ID: fmt.Sprintf("T%d", i), Member: "taker", Side: side, Kind: engine.Limit,
			Price: spec.PriceString(mid + lvl), Amount: spec.AmountString(int64(1 + rng.IntN(4))),
		}}})
		for _, ev := range evs {
			fills += len(ev.Fills)
			completed += len(ev.Completed)
		}
	}
	d1 := time.Since(t0)
	fmt.Printf("phase 1   : %6d limit orders in %s → %.0f orders/sec, %d fills, %d completions\n",
		N1, d1.Round(time.Millisecond), float64(N1)/d1.Seconds(), fills, completed)

	// ---- phase 2: tick-to-trade latency (batch of one, like a poll of size 1) ----
	const N2 = 20_000
	lat := make([]time.Duration, 0, N2)
	for i := range N2 {
		lvl := int64(rng.IntN(41) - 20)
		side := engine.Buy
		if i%2 == 1 {
			side = engine.Sell
		}
		s := time.Now()
		book.Batch([]engine.Command{{Place: &engine.PlaceIn{
			ID: fmt.Sprintf("L%d", i), Member: "taker", Side: side, Kind: engine.Limit,
			Price: spec.PriceString(mid + lvl), Amount: spec.AmountString(int64(1 + rng.IntN(4))),
		}}})
		lat = append(lat, time.Since(s))
	}
	fmt.Printf("phase 2   : tick-to-trade (single-cmd batch) p50=%s p95=%s p99=%s max=%s\n",
		pct(lat, 0.50), pct(lat, 0.95), pct(lat, 0.99), pct(lat, 1.0))

	// ---- phase 3: cancel storm over the resting book ----
	st := book.State()
	var cancelCmds []engine.Command
	for _, o := range st.OpenLimit {
		cancelCmds = append(cancelCmds, engine.Command{CancelID: o.ID})
		if len(cancelCmds) >= 20_000 {
			break
		}
	}
	t0 = time.Now()
	var canceled int
	for _, ev := range book.Batch(cancelCmds) {
		if ev.Canceled != nil {
			canceled++
		}
	}
	d3 := time.Since(t0)
	fmt.Printf("phase 3   : %6d cancels in %s → %.0f cancels/sec (%d found)\n",
		len(cancelCmds), d3.Round(time.Millisecond), float64(len(cancelCmds))/d3.Seconds(), canceled)

	// ---- phase 4: depth snapshot cost ----
	t0 = time.Now()
	const N4 = 100_000
	for range N4 {
		book.Plate(engine.Buy, 100)
	}
	d4 := time.Since(t0)
	fmt.Printf("phase 4   : %6d depth snapshots (100 levels) in %s → %.2f µs/snapshot\n",
		N4, d4.Round(time.Millisecond), float64(d4.Microseconds())/N4)

	final := book.State()
	fmt.Printf("book      : %d bid levels / %d ask levels / %d market orders resting, seq=%d\n",
		len(final.PlateBuy.Items), len(final.PlateSell.Items), len(final.OpenMkt), final.Seq)
}
