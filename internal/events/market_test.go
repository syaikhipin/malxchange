package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestMarketUpdatedTopicContract pins the shared topic contract (ticket
// #29): exchange owns market.updated, created at boot with no auto-create,
// one partition (per-Market ordering via the envelope key), and trade.fill
// retention so a rebuild replay converges.
func TestMarketUpdatedTopicContract(t *testing.T) {
	owned := OwnedTopics("exchange")
	for _, td := range owned {
		if td.Topic != TopicMarketUpdated {
			continue
		}
		if td.Partitions != 1 {
			t.Errorf("partitions = %d, want 1", td.Partitions)
		}
		if td.Retention != 7*24*time.Hour {
			t.Errorf("retention = %s, want 168h (matches trade.fill)", td.Retention)
		}
		return
	}
	t.Fatalf("exchange does not own %q: %+v", TopicMarketUpdated, owned)
}

// TestMarketUpdatedPayloadDecimalStrings pins the full-row wire shape: the
// complete markets row with every money value a decimal string (ADR-0005),
// "" meaning unbounded, and absent optional caps omitted.
func TestMarketUpdatedPayloadDecimalStrings(t *testing.T) {
	maxOpen := int32(10)
	ttl := int32(300)
	raw := Raw(MarketUpdated{
		Market: "BTC/USDT", BaseCoin: "BTC", QuoteCoin: "USDT",
		BaseScale: 8, QuoteScale: 2,
		TickSize: "0.1", LotSize: "0.0001",
		MakerFeeRate: "0.001", TakerFeeRate: "0.0015",
		MinPrice: "", MaxPrice: "1000000", MinAmount: "0", MaxAmount: "", MinTurnover: "5",
		MarketOrdersEnabled: true,
		Enabled:             true,
		Visible:             true,
		Sort:                1, Zone: "main", Featured: true,
		MaxOpenOrders: &maxOpen, TTLSeconds: &ttl,
	})
	want := `{"market":"BTC/USDT","baseCoin":"BTC","quoteCoin":"USDT","baseScale":8,"quoteScale":2,` +
		`"tickSize":"0.1","lotSize":"0.0001","makerFeeRate":"0.001","takerFeeRate":"0.0015",` +
		`"minPrice":"","maxPrice":"1000000","minAmount":"0","maxAmount":"","minTurnover":"5",` +
		`"marketOrdersEnabled":true,"enabled":true,"visible":true,` +
		`"sort":1,"zone":"main","featured":true,"maxOpenOrders":10,"ttlSeconds":300}`
	if string(raw) != want {
		t.Errorf("payload = %s\nwant    %s", raw, want)
	}

	raw = Raw(MarketUpdated{Market: "ETH/USDT"})
	want = `{"market":"ETH/USDT","baseCoin":"","quoteCoin":"","baseScale":0,"quoteScale":0,` +
		`"tickSize":"","lotSize":"","makerFeeRate":"","takerFeeRate":"",` +
		`"minPrice":"","maxPrice":"","minAmount":"","maxAmount":"","minTurnover":"",` +
		`"marketOrdersEnabled":false,"enabled":false,"visible":false,` +
		`"sort":0,"zone":"","featured":false}`
	if string(raw) != want {
		t.Errorf("nil-cap payload = %s\nwant            %s", raw, want)
	}
}

// TestPlateWireShapeDistinguishesEmptyFromUnchanged pins the Depth
// keep/clear wire contract: a dirty-but-empty side serializes as []
// (consumers clear that side) and an untouched side as null (consumers
// keep their last-known levels). While the sides carried omitempty the
// two shapes were indistinguishable — a fully swept side could never
// clear published Depth (audit issue #79).
func TestPlateWireShapeDistinguishesEmptyFromUnchanged(t *testing.T) {
	cleared := Plate{Market: "BTC/USDT", Bids: []PlateLevel{}}
	raw, err := json.Marshal(cleared)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"bids":[]`) {
		t.Fatalf("emptied side must serialize as []: %s", raw)
	}

	untouched := Plate{Market: "BTC/USDT"}
	raw, err = json.Marshal(untouched)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"bids":null`) || !strings.Contains(string(raw), `"asks":null`) {
		t.Fatalf("untouched sides must serialize as null: %s", raw)
	}

	var back Plate
	if err := json.Unmarshal(raw, &back); err != nil || back.Bids != nil || back.Asks != nil {
		t.Fatalf("null sides must decode to nil (keep): %+v err=%v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"market":"BTC/USDT","bids":[]}`), &back); err != nil || back.Bids == nil || len(back.Bids) != 0 {
		t.Fatalf("empty-array side must decode to non-nil empty (clear): %+v err=%v", back, err)
	}
}
