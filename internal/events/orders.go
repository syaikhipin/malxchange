package events

import "encoding/json"

// Wire data payloads for the order flow (issue #21/#24 contract). Money
// values are decimal strings (ADR-0005); envelope keys provide per-Market
// ordering on every topic below.

// OrderCommand is the data payload on TopicOrderCommand. Exactly one of
// Place/Cancel is set. The api service produces it after wallet freeze;
// the exchange engine consumes it.
type OrderCommand struct {
	Op      string `json:"op"` // "place" | "cancel" | "ttl-cancel"
	OrderID string `json:"orderId"`
	Member  string `json:"member"`
	Market  string `json:"market"`

	// place only:
	Side   string `json:"side,omitempty"`   // "buy" | "sell"
	Kind   string `json:"kind,omitempty"`   // "limit" | "market"
	Price  string `json:"price,omitempty"`  // limit only, decimal string
	Amount string `json:"amount,omitempty"` // base amount, or quote budget for market buy
}

// TradeFill is the self-contained fill event on TopicTradeFill (ADR-0009:
// wallet settles from this alone — no exchange-schema joins). Fees are
// charged in the Coin each side receives (buy: base, sell: quote),
// floored per fill at fill-time rates.
type TradeFill struct {
	Market       string `json:"market"`
	Seq          uint64 `json:"seq"`
	BuyOrderID   string `json:"buyOrderId"`
	SellOrderID  string `json:"sellOrderId"`
	BuyMemberID  string `json:"buyMemberId"`
	SellMemberID string `json:"sellMemberId"`
	TakerSide    string `json:"takerSide"`
	Price        string `json:"price"`
	Amount       string `json:"amount"`
	Turnover     string `json:"turnover"`
	BuyTurnover  string `json:"buyTurnover"` // includes burned dust
	BuyFee       string `json:"buyFee"`      // in base Coin
	SellFee      string `json:"sellFee"`     // in quote Coin
}

// OrderCompleted is the terminal-state event on TopicOrderCompleted.
type OrderCompleted struct {
	Market       string `json:"market"`
	OrderID      string `json:"orderId"`
	MemberID     string `json:"memberId"`
	Side         string `json:"side"`
	Kind         string `json:"kind"`
	Status       string `json:"status"` // "filled" | "canceled"
	FilledAmount string `json:"filledAmount"`
	Turnover     string `json:"turnover"`
}

// CancelRejected answers a cancel naming an order the engine doesn't know.
type CancelRejected struct {
	Market  string `json:"market"`
	OrderID string `json:"orderId"`
	Member  string `json:"member"`
	Reason  string `json:"reason"`
}

// PlateLevel is one aggregated depth level.
type PlateLevel struct {
	Price  string `json:"price"`
	Amount string `json:"amount"`
}

// Plate is the Depth snapshot on TopicMarketPlate: whichever sides changed
// in the batch (engine publishes at most once per batch per side). The
// sides deliberately keep omitempty OFF: a dirty-but-empty side serializes
// as [] (the consumer clears that side's Depth), an unchanged side stays
// nil and serializes as null (the consumer keeps its last-known levels) —
// absent and empty are different wire shapes and must stay distinguishable
// (audit issue #79).
type Plate struct {
	Market string       `json:"market"`
	Bids   []PlateLevel `json:"bids"` // best first
	Asks   []PlateLevel `json:"asks"` // best first
}

// RawMessage helpers keep the producers one-liners.
func Raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // structs of strings cannot fail to marshal
	}
	return b
}
