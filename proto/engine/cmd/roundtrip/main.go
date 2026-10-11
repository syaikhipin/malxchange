// THROWAWAY PROTOTYPE (branch proto/engine). Kafka in/out round-trip for the
// ported engine, using kfake: an in-process broker that speaks the real Kafka
// protocol, driven by franz-go — zero infrastructure. A real Redpanda container
// is validated separately in the install-stack work.
//
// Flow: producer → topic order.command (keyed per market, so partition order
// is deterministic) → engine consumer (poll batch → Book.Batch → produce
// fill / order.completed / plate events) → verifier consumes everything back
// and compares against a direct in-process run of the same command stream.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	engine "github.com/syaikhipin/malxchange/proto/engine/engine"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

var spec = engine.MarketSpec{
	Symbol: "BTC/USDT", BaseScale: 8, QuoteScale: 2,
	TickRaw: 1, LotRaw: 10000,
}

const (
	topicCmd   = "order.command"
	topicFill  = "trade.fill"
	topicDone  = "order.completed"
	topicPlate = "market.plate"

	numSeedMaker = 2000 // makers per side
	numTakers    = 8000
)

// commandMsg is the wire form on order.command.
type commandMsg struct {
	ID     string `json:"id"`
	Member string `json:"member"`
	Side   string `json:"side"`
	Kind   string `json:"kind"`
	Price  string `json:"price"`
	Amount string `json:"amount"`
	Cancel string `json:"cancel,omitempty"`
}

type fillMsg struct {
	Seq            uint64 `json:"seq"`
	BuyOrderID     string `json:"buy"`
	SellOrderID    string `json:"sell"`
	TakerSide      string `json:"takerSide"`
	PriceTicks     int64  `json:"priceTicks"`
	AmountLots     int64  `json:"amountLots"`
	TurnoverRaw    int64  `json:"turnoverRaw"`
	BuyTurnoverRaw int64  `json:"buyTurnoverRaw"`
}

// commandStream: seed makers on both sides, then crossing takers, occasional
// market orders with dust-prone budgets, and cancels of earlier orders.
func commandStream() []commandMsg {
	var cmds []commandMsg
	placed := 0
	add := func(m commandMsg) { cmds = append(cmds, m) }
	for i := range numSeedMaker {
		lvl := int64(10 + i%400)
		add(commandMsg{ID: fmt.Sprintf("O%d", placed), Member: "maker", Side: "sell",
			Price: spec.PriceString(50000_00 + lvl), Amount: spec.AmountString(int64(1 + i%5))})
		placed++
		add(commandMsg{ID: fmt.Sprintf("O%d", placed), Member: "maker", Side: "buy",
			Price: spec.PriceString(50000_00 - lvl), Amount: spec.AmountString(int64(1 + i%5))})
		placed++
	}
	for i := range numTakers {
		id := fmt.Sprintf("O%d", placed)
		placed++
		switch {
		case i%20 == 19: // market buy with an odd budget → dust burn paths
			add(commandMsg{ID: id, Member: "taker", Side: "buy", Kind: "market",
				Amount: spec.MoneyString(int64(50 + i%400))})
		default: // limit takers straddling the mid (lvl in [-4, 4])
			lvl := int64(i%9 - 4)
			side := "buy"
			if i%2 == 1 {
				side = "sell"
			}
			add(commandMsg{ID: id, Member: "taker", Side: side,
				Price: spec.PriceString(50000_00 + lvl), Amount: spec.AmountString(int64(1 + i%3))})
			if i%25 == 24 {
				add(commandMsg{Cancel: fmt.Sprintf("O%d", placed-13)})
			}
		}
	}
	return cmds
}

func (m commandMsg) toCommand() engine.Command {
	if m.Cancel != "" {
		return engine.Command{CancelID: m.Cancel}
	}
	side, kind := engine.Buy, engine.Limit
	if m.Side == "sell" {
		side = engine.Sell
	}
	if m.Kind == "market" {
		kind = engine.Market
	}
	return engine.Command{Place: &engine.PlaceIn{ID: m.ID, Member: m.Member, Side: side, Kind: kind, Price: m.Price, Amount: m.Amount}}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func mark(ok bool) string {
	if ok {
		return "OK"
	}
	return "MISMATCH"
}

func stateEqual(a, b engine.State) bool {
	if a.Seq != b.Seq || len(a.OpenLimit) != len(b.OpenLimit) || len(a.OpenMkt) != len(b.OpenMkt) {
		return false
	}
	plateEq := func(x, y *engine.Plate) bool {
		if (x == nil) != (y == nil) || x == nil || len(x.Items) != len(y.Items) {
			return false
		}
		for i := range x.Items {
			if x.Items[i] != y.Items[i] {
				return false
			}
		}
		return true
	}
	return plateEq(a.PlateBuy, b.PlateBuy) && plateEq(a.PlateSell, b.PlateSell)
}

func main() {
	ctx := context.Background()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		panic(err)
	}
	defer cluster.Close()
	seed := cluster.ListenAddrs()[0]
	for _, t := range []string{topicCmd, topicFill, topicDone, topicPlate} {
		if err := cluster.CreateTopic(t, 1, nil); err != nil {
			panic(err)
		}
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(seed))
	if err != nil {
		panic(err)
	}
	defer cl.Close()
	cmds := commandStream()

	// Ground truth: the same stream straight through a Book, no Kafka.
	direct := engine.NewBook(spec)
	var directFills int
	var directDone int
	var directTurnover int64
	for _, m := range cmds {
		for _, ev := range direct.Batch([]engine.Command{m.toCommand()}) {
			directFills += len(ev.Fills)
			directDone += len(ev.Completed)
			for _, f := range ev.Fills {
				directTurnover += f.TurnoverRaw
			}
		}
	}

	// Produce all commands, keyed per market (single partition → total order).
	start := time.Now()
	for _, m := range cmds {
		cl.Produce(ctx, &kgo.Record{Topic: topicCmd, Key: []byte(spec.Symbol), Value: mustJSON(m)}, nil)
	}
	cl.Flush(ctx)

	// Engine consumer: poll batch → Book.Batch → produce events.
	consumer, err := kgo.NewClient(kgo.SeedBrokers(seed), kgo.ConsumerGroup("engine-roundtrip"),
		kgo.ConsumeTopics(topicCmd), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		panic(err)
	}
	defer consumer.Close()
	engineBook := engine.NewBook(spec)
	consumed, batches, fillsK, doneK, platesK := 0, 0, 0, 0, 0
	var turnoverK int64
	var lastBatchEnd time.Time
	pollCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for consumed < len(cmds) {
		fetches := consumer.PollRecords(pollCtx, 500)
		if errs := fetches.Errors(); len(errs) > 0 {
			fmt.Println("fetch error:", errs)
			if pollCtx.Err() != nil {
				panic(fmt.Sprintf("consumer context expired after consuming %d/%d", consumed, len(cmds)))
			}
		}
		iter := fetches.RecordIter()
		var batch []engine.Command
		for !iter.Done() {
			rec := iter.Next()
			var m commandMsg
			if err := json.Unmarshal(rec.Value, &m); err != nil {
				panic(err)
			}
			batch = append(batch, m.toCommand())
		}
		if len(batch) == 0 {
			continue
		}
		consumed += len(batch)
		batches++
		for _, ev := range engineBook.Batch(batch) {
			for _, f := range ev.Fills {
				turnoverK += f.TurnoverRaw
				fillsK++
				cl.Produce(ctx, &kgo.Record{Topic: topicFill, Value: mustJSON(fillMsg{
					f.Seq, f.BuyOrderID, f.SellOrderID, f.TakerSide.String(), f.PriceTicks, f.AmountLots, f.TurnoverRaw, f.BuyTurnoverRaw,
				})}, nil)
			}
			for _, c := range ev.Completed {
				doneK++
				cl.Produce(ctx, &kgo.Record{Topic: topicDone, Value: mustJSON(c.ID)}, nil)
			}
			if ev.PlateBuy != nil {
				platesK++
				cl.Produce(ctx, &kgo.Record{Topic: topicPlate, Key: []byte("buy"), Value: []byte(fmt.Sprintf("buy:%d levels", len(ev.PlateBuy.Items)))}, nil)
			}
			if ev.PlateSell != nil {
				platesK++
				cl.Produce(ctx, &kgo.Record{Topic: topicPlate, Key: []byte("sell"), Value: []byte(fmt.Sprintf("sell:%d levels", len(ev.PlateSell.Items)))}, nil)
			}
		}
		lastBatchEnd = time.Now()
	}
	cl.Flush(ctx)
	el := lastBatchEnd.Sub(start)

	// Verifier: read the event topics back from the start and recount.
	verifier, err := kgo.NewClient(kgo.SeedBrokers(seed),
		kgo.ConsumeTopics(topicFill, topicDone, topicPlate),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		panic(err)
	}
	vFills, vDone, vPlates := 0, 0, 0
	var vTurnover int64
	vCtx, vCancel := context.WithTimeout(ctx, 30*time.Second)
	for (vFills < fillsK || vDone < doneK || vPlates < platesK) && vCtx.Err() == nil {
		fetches := verifier.PollRecords(vCtx, 500)
		iter := fetches.RecordIter()
		for !iter.Done() {
			r := iter.Next()
			switch r.Topic {
			case topicFill:
				var f fillMsg
				if err := json.Unmarshal(r.Value, &f); err != nil {
					panic(err)
				}
				vFills++
				vTurnover += f.TurnoverRaw
			case topicDone:
				vDone++
			case topicPlate:
				vPlates++
			}
		}
	}
	vCancel()
	verifier.Close()

	ok := vFills == directFills && vDone == directDone && vTurnover == directTurnover && stateEqual(engineBook.State(), direct.State())
	fmt.Printf(`
Kafka round-trip via kfake (in-process Kafka-protocol broker, franz-go client)
  commands produced .......... %d
  consumer batches ........... %d (avg %d cmds/batch)
  fills: engine/direct/back .. %d / %d / %d   %s
  completions: back/direct ... %d / %d        %s
  plate publishes ............ %d
  turnover: back/direct ...... %d / %d raw   %s
  final book state ........... %s
  produce → last batch ....... %s (%.0f cmds/sec end-to-end)
`, len(cmds), batches, len(cmds)/max(batches, 1),
		fillsK, directFills, vFills, mark(fillsK == directFills && vFills == directFills),
		vDone, directDone, mark(vDone == directDone),
		platesK,
		vTurnover, directTurnover, mark(vTurnover == directTurnover),
		mark(stateEqual(engineBook.State(), direct.State())),
		el, float64(len(cmds))/el.Seconds())
	if !ok {
		fmt.Println("  MISMATCH — Kafka path diverged from the direct run")
		os.Exit(1)
	}
}
