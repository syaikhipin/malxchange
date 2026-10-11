// Package engine is a THROWAWAY PROTOTYPE (branch proto/engine, never merged).
//
// Question it answers: do the ported CoinTrader.java matching semantics feel
// right in Go, with the book operating on int64 ticks/lots per ADR-0005?
//
// Port source: 00_framework/exchange/.../Trader/CoinTrader.java
// Deliberate divergences from the legacy (flagged for the engine spec ticket):
//  1. Cancel is by order ID only (legacy needed side+type+price hints);
//     the book keeps an ID index.
//  2. Plates are published once per batch per side (legacy re-published the
//     full plate on every mutation).
//  3. FENTAN (pro-rated publish mode) is not ported.
//  4. Fill economics (turnover = price × amount, affordable-lots division)
//     use math/big on the cold path; the hot book path is pure int64 —
//     raw-scale cross products overflow int64 (e.g. 3000.00 USDT raw ×
//     1e14 lot raw ≈ 3e19 > 2^63), exactly the ADR-0005 caveat.
package engine

import (
	"fmt"
	"math/big"
	"strings"
)

// Side of an Order.
type Side int8

const (
	Buy Side = iota
	Sell
)

func (s Side) String() string {
	if s == Buy {
		return "buy"
	}
	return "sell"
}

// Kind of an Order. Market buys spend a quote budget; everything else
// trades a base quantity (legacy semantics).
type Kind int8

const (
	Limit Kind = iota
	Market
)

func (k Kind) String() string {
	if k == Limit {
		return "limit"
	}
	return "market"
}

// MarketSpec is the per-Market quantization config (ADR-0005).
type MarketSpec struct {
	Symbol     string
	BaseScale  int   // decimal places of the base Coin (BTC 8)
	QuoteScale int   // decimal places of the quote Coin (USDT 2)
	TickRaw    int64 // tick size in quote raw units (0.01 USDT → 1)
	LotRaw     int64 // lot size in base raw units (0.0001 BTC → 10000)
}

// Fill is the execution of some amount of an Order against another Order
// (glossary term; legacy ExchangeTrade). The taker side is recorded.
type Fill struct {
	Seq            uint64
	BuyOrderID     string
	SellOrderID    string
	TakerSide      Side
	PriceTicks     int64
	AmountLots     int64
	TurnoverRaw    int64 // quote raw
	BuyTurnoverRaw int64 // includes burned dust when it differs
}

// PlateItem is one aggregated depth level.
type PlateItem struct {
	PriceTicks int64
	AmountLots int64 // total remaining at this level
}

// Plate is a depth snapshot (legacy TradePlate, max depth 100).
type Plate struct {
	Side  Side
	Items []PlateItem
}

type order struct {
	ID          string
	MemberID    string
	Side        Side
	Kind        Kind
	PriceTicks  int64 // limit only
	AmountLots  int64 // limit + market sell
	BudgetRaw   int64 // market buy: quote budget
	TradedLots  int64
	TurnoverRaw int64
	Seq         uint64 // arrival, for time priority
}

func (o *order) completed() bool {
	if o.Kind == Market && o.Side == Buy {
		return o.TurnoverRaw >= o.BudgetRaw
	}
	return o.TradedLots >= o.AmountLots
}

type level struct {
	price  int64
	orders []*order // FIFO within price
}

type loc struct {
	side  Side // resting side
	kind  Kind
	price int64
	mkt   bool
}

// Book is the per-Market matching engine. Not concurrency-safe by design:
// one goroutine owns it, exactly like the legacy single CoinTrader thread.
type Book struct {
	spec      MarketSpec
	buy       []*level // price desc
	sell      []*level // price asc
	buyMkt    []*order
	sellMkt   []*order
	index     map[string]*loc
	halted    bool
	seq       uint64
	dirtyBuy  bool
	dirtySell bool
}

func NewBook(spec MarketSpec) *Book {
	return &Book{spec: spec, index: map[string]*loc{}}
}

// Event is what one command produced. Plates are attached at batch end.
type Event struct {
	Placed    *PlacedOrder
	Rejected  *Rejection
	Canceled  *PlacedOrder
	Fills     []Fill
	Completed []*PlacedOrder
	PlateBuy  *Plate
	PlateSell *Plate
}

// PlacedOrder is the caller-facing projection of an order.
type PlacedOrder struct {
	ID          string
	MemberID    string
	Side        Side
	Kind        Kind
	PriceTicks  int64
	AmountLots  int64
	BudgetRaw   int64
	TradedLots  int64
	TurnoverRaw int64
	Seq         uint64
	Waiting     bool // still resting on the book
}

type Rejection struct {
	OrderID string
	Reason  string
}

// Command is one batched input. At most one field set.
type Command struct {
	Place    *PlaceIn
	CancelID string
	Halt     bool
	Resume   bool
	Reset    bool
}

// PlaceIn carries decimal strings, quantized at the boundary
// (rejections here mirror what the api service will enforce).
type PlaceIn struct {
	ID     string
	Member string
	Side   Side
	Kind   Kind
	Price  string // limit only
	Amount string // base lots, or quote budget for market buy
}

func (b *Book) snapshot(o *order, waiting bool) *PlacedOrder {
	return &PlacedOrder{
		ID: o.ID, MemberID: o.MemberID, Side: o.Side, Kind: o.Kind,
		PriceTicks: o.PriceTicks, AmountLots: o.AmountLots, BudgetRaw: o.BudgetRaw,
		TradedLots: o.TradedLots, TurnoverRaw: o.TurnoverRaw, Seq: o.Seq, Waiting: waiting,
	}
}

// Batch applies commands in order and returns one Event per command plus a
// final synthetic event carrying at most one plate snapshot per dirty side.
func (b *Book) Batch(cmds []Command) []Event {
	evs := make([]Event, 0, len(cmds)+1)
	for _, c := range cmds {
		switch {
		case c.Reset != false || c.Place == nil && c.CancelID == "" && !c.Halt && !c.Resume:
			if c.Reset {
				*b = *NewBook(b.spec)
			}
		case c.Place != nil:
			evs = append(evs, b.place(c.Place))
		case c.CancelID != "":
			evs = append(evs, b.cancel(c.CancelID))
		case c.Halt:
			b.halted = true
		case c.Resume:
			b.halted = false
		}
	}
	var tail Event
	if b.dirtyBuy {
		tail.PlateBuy = b.Plate(Buy, 100)
	}
	if b.dirtySell {
		tail.PlateSell = b.Plate(Sell, 100)
	}
	if tail.PlateBuy != nil || tail.PlateSell != nil {
		evs = append(evs, tail)
	}
	b.dirtyBuy, b.dirtySell = false, false
	return evs
}

func (b *Book) place(in *PlaceIn) Event {
	o, err := b.quantize(in)
	if err != nil {
		return Event{Rejected: &Rejection{OrderID: in.ID, Reason: err.Error()}}
	}
	if b.halted {
		return Event{Rejected: &Rejection{OrderID: in.ID, Reason: "trading is halted"}}
	}
	o.Seq = b.nextSeq()
	ev := Event{Placed: b.snapshot(o, true)}
	b.take(o, &ev)
	return ev
}

func (b *Book) quantize(in *PlaceIn) (*order, error) {
	o := &order{ID: in.ID, MemberID: in.Member, Side: in.Side, Kind: in.Kind}
	switch {
	case in.Kind == Limit:
		pRaw, ok := parseRaw(in.Price, b.spec.QuoteScale)
		if !ok || pRaw <= 0 {
			return nil, fmt.Errorf("price must be a positive decimal")
		}
		if pRaw%b.spec.TickRaw != 0 {
			return nil, fmt.Errorf("price %s is not a multiple of the tick size", in.Price)
		}
		aRaw, ok := parseRaw(in.Amount, b.spec.BaseScale)
		if !ok || aRaw <= 0 {
			return nil, fmt.Errorf("amount must be a positive decimal")
		}
		if aRaw%b.spec.LotRaw != 0 {
			return nil, fmt.Errorf("amount %s is not a multiple of the lot size", in.Amount)
		}
		o.PriceTicks = pRaw / b.spec.TickRaw
		o.AmountLots = aRaw / b.spec.LotRaw
	case in.Kind == Market && in.Side == Sell:
		aRaw, ok := parseRaw(in.Amount, b.spec.BaseScale)
		if !ok || aRaw <= 0 {
			return nil, fmt.Errorf("amount must be a positive decimal")
		}
		if aRaw%b.spec.LotRaw != 0 {
			return nil, fmt.Errorf("amount %s is not a multiple of the lot size", in.Amount)
		}
		o.AmountLots = aRaw / b.spec.LotRaw
	default: // market buy: amount is the quote budget
		raw, ok := parseRaw(in.Amount, b.spec.QuoteScale)
		if !ok || raw <= 0 {
			return nil, fmt.Errorf("budget must be a positive decimal")
		}
		o.BudgetRaw = raw
	}
	return o, nil
}

// take runs the taker through the book (legacy CoinTrader.trade).
func (b *Book) take(taker *order, ev *Event) {
	switch taker.Kind {
	case Market:
		b.matchWithLimitLevels(taker, true, ev)
		if !taker.completed() {
			b.addMarket(taker)
		}
	case Limit:
		if taker.PriceTicks <= 0 {
			ev.Rejected = &Rejection{OrderID: taker.ID, Reason: "limit price must be positive"}
			return
		}
		b.matchWithLimitLevels(taker, false, ev)
		if !taker.completed() {
			b.matchWithMarketQueue(taker, ev)
		}
		if !taker.completed() {
			b.addLimit(taker)
		}
	}
	if taker.completed() {
		ev.Completed = append(ev.Completed, b.snapshot(taker, false))
	}
}

// matchWithLimitLevels walks the opposite limit levels best-price-first.
// Market takers have no price bound (they sweep); limit takers stop at
// their own price (buy: level price above taker price → stop).
func (b *Book) matchWithLimitLevels(taker *order, unbounded bool, ev *Event) {
	var levels *[]*level
	if taker.Side == Buy {
		levels = &b.sell
	} else {
		levels = &b.buy
	}
	for li := 0; li < len(*levels); {
		lv := (*levels)[li]
		if !unbounded {
			if taker.Side == Buy && lv.price > taker.PriceTicks {
				break
			}
			if taker.Side == Sell && lv.price < taker.PriceTicks {
				break
			}
		}
		for oi := 0; oi < len(lv.orders); {
			rest := lv.orders[oi]
			qty := b.fillAgainst(taker, rest, lv.price, ev)
			if rest.completed() {
				lv.orders = append(lv.orders[:oi], lv.orders[oi+1:]...)
				b.unindex(rest)
				ev.Completed = append(ev.Completed, b.snapshot(rest, false))
			} else {
				oi++
			}
			if qty > 0 {
				b.markPlateDirty(rest.Side)
			}
			if taker.completed() {
				return
			}
		}
		if len(lv.orders) == 0 {
			*levels = append((*levels)[:li], (*levels)[li+1:]...)
		} else {
			li++
		}
	}
}

// matchWithMarketQueue: a leftover limit taker eats resting market orders
// FIFO at the taker's own price (legacy matchLimitPriceWithMPList).
func (b *Book) matchWithMarketQueue(taker *order, ev *Event) {
	var q *[]*order
	if taker.Side == Buy {
		q = &b.sellMkt
	} else {
		q = &b.buyMkt
	}
	for qi := 0; qi < len(*q); {
		rest := (*q)[qi]
		b.fillAgainst(taker, rest, taker.PriceTicks, ev)
		if rest.completed() {
			*q = append((*q)[:qi], (*q)[qi+1:]...)
			b.unindex(rest)
			ev.Completed = append(ev.Completed, b.snapshot(rest, false))
		} else {
			qi++
		}
		if taker.completed() {
			return
		}
	}
}

// fillAgainst executes one taker/resting pair at dealPrice. Returns the
// filled quantity (0 = no fill possible, legacy's null trade).
func (b *Book) fillAgainst(taker, rest *order, dealPrice int64, ev *Event) int64 {
	need := b.needLots(taker, dealPrice)
	avail := b.needLots(rest, dealPrice)
	qty := need
	if avail < qty {
		qty = avail
	}
	if qty <= 0 || dealPrice <= 0 {
		return 0
	}
	turn := b.spec.turnoverRaw(dealPrice, qty)
	taker.TradedLots += qty
	rest.TradedLots += qty
	taker.TurnoverRaw += turn
	rest.TurnoverRaw += turn

	f := Fill{
		Seq: b.nextSeq(), PriceTicks: dealPrice, AmountLots: qty,
		TurnoverRaw: turn, BuyTurnoverRaw: turn, TakerSide: taker.Side,
	}
	if taker.Side == Buy {
		f.BuyOrderID, f.SellOrderID = taker.ID, rest.ID
	} else {
		f.BuyOrderID, f.SellOrderID = rest.ID, taker.ID
	}
	// Dust burn (legacy adjustMarketOrderTurnover): a market buy whose
	// remaining budget cannot buy one more lot at this price is completed
	// and the leftover is folded into this fill's buy turnover.
	for _, o := range [2]*order{taker, rest} {
		if o.Kind == Market && o.Side == Buy && !o.completed() {
			if b.spec.affordableLots(o.BudgetRaw-o.TurnoverRaw, dealPrice) == 0 {
				left := o.BudgetRaw - o.TurnoverRaw
				o.TurnoverRaw = o.BudgetRaw
				f.BuyTurnoverRaw = turn + left
			}
		}
	}
	ev.Fills = append(ev.Fills, f)
	return qty
}

func (b *Book) needLots(o *order, price int64) int64 {
	if o.Kind == Market && o.Side == Buy {
		return b.spec.affordableLots(o.BudgetRaw-o.TurnoverRaw, price)
	}
	return o.AmountLots - o.TradedLots
}

func (b *Book) addLimit(o *order) {
	var levels *[]*level
	if o.Side == Buy {
		levels = &b.buy
	} else {
		levels = &b.sell
	}
	i := sortLevel(*levels, o.PriceTicks, o.Side)
	if i < len(*levels) && (*levels)[i].price == o.PriceTicks {
		(*levels)[i].orders = append((*levels)[i].orders, o)
	} else {
		*levels = append(*levels, nil)
		copy((*levels)[i+1:], (*levels)[i:])
		(*levels)[i] = &level{price: o.PriceTicks, orders: []*order{o}}
	}
	b.index[o.ID] = &loc{side: o.Side, kind: Limit, price: o.PriceTicks}
	b.markPlateDirty(o.Side)
}

func (b *Book) addMarket(o *order) {
	if o.Side == Buy {
		b.buyMkt = append(b.buyMkt, o)
	} else {
		b.sellMkt = append(b.sellMkt, o)
	}
	b.index[o.ID] = &loc{side: o.Side, kind: Market, mkt: true}
}

func sortLevel(levels []*level, price int64, side Side) int {
	lo, hi := 0, len(levels)
	for lo < hi {
		mid := (lo + hi) / 2
		better := side == Buy && levels[mid].price > price || side == Sell && levels[mid].price < price
		if better {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func (b *Book) cancel(id string) Event {
	l, ok := b.index[id]
	if !ok {
		return Event{}
	}
	if l.mkt {
		var q *[]*order
		if l.side == Buy {
			q = &b.buyMkt
		} else {
			q = &b.sellMkt
		}
		for i, o := range *q {
			if o.ID == id {
				*q = append((*q)[:i], (*q)[i+1:]...)
				delete(b.index, id)
				return Event{Canceled: b.snapshot(o, false)}
			}
		}
	}
	var levels *[]*level
	if l.side == Buy {
		levels = &b.buy
	} else {
		levels = &b.sell
	}
	for li, lv := range *levels {
		if lv.price != l.price {
			continue
		}
		for i, o := range lv.orders {
			if o.ID == id {
				lv.orders = append(lv.orders[:i], lv.orders[i+1:]...)
				if len(lv.orders) == 0 {
					*levels = append((*levels)[:li], (*levels)[li+1:]...)
				}
				delete(b.index, id)
				b.markPlateDirty(l.side)
				return Event{Canceled: b.snapshot(o, false)}
			}
		}
	}
	return Event{}
}

func (b *Book) markPlateDirty(side Side) {
	if side == Buy {
		b.dirtyBuy = true
	} else {
		b.dirtySell = true
	}
}

func (b *Book) unindex(o *order) {
	delete(b.index, o.ID)
}

func (b *Book) nextSeq() uint64 {
	b.seq++
	return b.seq
}

// Plate builds a depth snapshot; market orders never appear (legacy rule).
func (b *Book) Plate(side Side, depth int) *Plate {
	var levels []*level
	if side == Buy {
		levels = b.buy
	} else {
		levels = b.sell
	}
	p := &Plate{Side: side}
	for _, lv := range levels {
		if len(p.Items) >= depth {
			break
		}
		var amt int64
		for _, o := range lv.orders {
			amt += o.AmountLots - o.TradedLots
		}
		if amt > 0 {
			p.Items = append(p.Items, PlateItem{PriceTicks: lv.price, AmountLots: amt})
		}
	}
	return p
}

// State is the full surfaced state (demo + verifier use it).
type State struct {
	Spec      MarketSpec
	Halted    bool
	PlateBuy  *Plate
	PlateSell *Plate
	OpenLimit []*PlacedOrder
	OpenMkt   []*PlacedOrder
	Seq       uint64
}

func (b *Book) State() State {
	st := State{Spec: b.spec, Halted: b.halted, PlateBuy: b.Plate(Buy, 100), PlateSell: b.Plate(Sell, 100), Seq: b.seq}
	for _, lv := range append(append([]*level{}, b.buy...), b.sell...) {
		for _, o := range lv.orders {
			st.OpenLimit = append(st.OpenLimit, b.snapshot(o, true))
		}
	}
	for _, q := range [][]*order{b.buyMkt, b.sellMkt} {
		for _, o := range q {
			st.OpenMkt = append(st.OpenMkt, b.snapshot(o, true))
		}
	}
	return st
}

// ---- decimal plumbing (display + boundary quantization only) ----

func (s MarketSpec) PriceString(ticks int64) string {
	return rawString(ticks*s.TickRaw, s.QuoteScale)
}

func (s MarketSpec) AmountString(lots int64) string {
	return rawString(lots*s.LotRaw, s.BaseScale)
}

func (s MarketSpec) MoneyString(raw int64) string {
	return rawString(raw, s.QuoteScale)
}

func rawString(raw int64, scale int) string {
	neg := raw < 0
	if neg {
		raw = -raw
	}
	d := fmt.Sprintf("%0*d", scale+1, raw)
	if scale == 0 {
		if neg {
			return "-" + d
		}
		return d
	}
	out := d[:len(d)-scale] + "." + d[len(d)-scale:]
	out = strings.TrimRight(out, "0")
	out = strings.TrimRight(out, ".")
	if neg {
		return "-" + out
	}
	return out
}

// parseRaw parses a non-negative decimal string at a scale.
func parseRaw(s string, scale int) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if len(fracPart) > scale {
		// more precision than the coin allows → not representable
		if strings.Trim(fracPart[scale:], "0") != "" {
			return 0, false
		}
		fracPart = fracPart[:scale]
	}
	fracPart += strings.Repeat("0", scale-len(fracPart))
	if intPart == "" {
		intPart = "0"
	}
	whole, ok := new(big.Int).SetString(intPart, 10)
	if !ok {
		return 0, false
	}
	frac, ok := new(big.Int).SetString(fracPart, 10)
	if !ok {
		return 0, false
	}
	whole.Mul(whole, pow10(scale))
	whole.Add(whole, frac)
	if !whole.IsInt64() {
		return 0, false
	}
	return whole.Int64(), true
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// turnoverRaw = floor(priceTicks × amountLots × TickRaw × LotRaw / 10^BaseScale).
func (s MarketSpec) turnoverRaw(priceTicks, amountLots int64) int64 {
	v := new(big.Int).Mul(big.NewInt(priceTicks), big.NewInt(amountLots))
	v.Mul(v, big.NewInt(s.TickRaw))
	v.Mul(v, big.NewInt(s.LotRaw))
	v.Div(v, pow10(s.BaseScale))
	return v.Int64()
}

// affordableLots = floor(budgetRaw × 10^BaseScale / (priceTicks × TickRaw × LotRaw)).
func (s MarketSpec) affordableLots(budgetRaw, priceTicks int64) int64 {
	num := new(big.Int).Mul(big.NewInt(budgetRaw), pow10(s.BaseScale))
	den := new(big.Int).Mul(big.NewInt(priceTicks), big.NewInt(s.TickRaw))
	den.Mul(den, big.NewInt(s.LotRaw))
	return new(big.Int).Quo(num, den).Int64()
}
