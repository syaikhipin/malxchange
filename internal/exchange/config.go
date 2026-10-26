// Package exchange wraps the pure engine with the production loop
// (ADR-0010): consume order.command, fold through per-Market Books,
// persist each batch in ONE Postgres transaction (order rows, fill rows,
// outbox events), commit Kafka offsets only after that transaction.
// Postgres is exactly the fold state at the last committed offset.
package exchange

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/syaikhipin/malxchange/internal/engine"
)

// MarketCfg is a markets-table row, resolved into engine quantization.
type MarketCfg struct {
	// Market is the glossary/wire name; the storage column keeps the
	// spec-#21 name `markets.symbol`.
	// ADR-0009 waiver — `markets.symbol` keeps the legacy column name
	// (spec #21); new identifiers use Market per GLOSSARY.
	Market       string
	BaseCoin     string
	QuoteCoin    string
	BaseScale    int
	QuoteScale   int
	TickRaw      int64
	LotRaw       int64
	TickSize     string // decimal strings, echo back to admin/front
	LotSize      string
	MakerFee     string
	TakerFee     string
	MinPrice     string // "" = unbounded
	MaxPrice     string
	MinAmount    string
	MaxAmount    string
	MinTurnover  string
	MarketOrders bool
	Enabled      bool
	Visible      bool
	Sort         int
	Zone         string
	Featured     bool
	TTLSeconds   *int32
	MaxOpen      *int32
}

// Spec returns the engine's quantization view of the Market.
func (m MarketCfg) Spec() engine.MarketSpec {
	return engine.MarketSpec{
		Symbol: m.Market, BaseScale: m.BaseScale, QuoteScale: m.QuoteScale,
		TickRaw: m.TickRaw, LotRaw: m.LotRaw,
	}
}

// quantizationChanged reports whether the engine-critical fields moved.
type quantization struct {
	tick, lot             int64
	baseScale, quoteScale int
}

func (m MarketCfg) quant() quantization {
	return quantization{m.TickRaw, m.LotRaw, m.BaseScale, m.QuoteScale}
}

// changed reports whether any config field differs from n, comparing the
// optional caps by value. Derived fields (TickRaw/LotRaw) follow from the
// decimal strings, and Postgres normalizes equal NUMERICs to equal text,
// so string equality is exact.
func (m MarketCfg) changed(n MarketCfg) bool {
	return m.BaseCoin != n.BaseCoin || m.QuoteCoin != n.QuoteCoin ||
		m.BaseScale != n.BaseScale || m.QuoteScale != n.QuoteScale ||
		m.TickSize != n.TickSize || m.LotSize != n.LotSize ||
		m.MakerFee != n.MakerFee || m.TakerFee != n.TakerFee ||
		m.MinPrice != n.MinPrice || m.MaxPrice != n.MaxPrice ||
		m.MinAmount != n.MinAmount || m.MaxAmount != n.MaxAmount ||
		m.MinTurnover != n.MinTurnover ||
		m.MarketOrders != n.MarketOrders || m.Enabled != n.Enabled || m.Visible != n.Visible ||
		m.Sort != n.Sort || m.Zone != n.Zone || m.Featured != n.Featured ||
		!int32Eq(m.MaxOpen, n.MaxOpen) || !int32Eq(m.TTLSeconds, n.TTLSeconds)
}

func int32Eq(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// LoadMarkets reads all market configs.
func LoadMarkets(ctx context.Context, pool *pgxpool.Pool) (map[string]MarketCfg, error) {
	rows, err := pool.Query(ctx, `
		SELECT symbol, base_coin, quote_coin, base_scale, quote_scale,
		       tick_size::text, lot_size::text,
		       maker_fee_rate::text, taker_fee_rate::text,
		       COALESCE(min_price::text,''), COALESCE(max_price::text,''),
		       min_amount::text, COALESCE(max_amount::text,''), min_turnover::text,
		       market_orders_enabled, enabled, visible, sort, zone, featured,
		       max_open_orders, ttl_seconds
		FROM markets`)
	if err != nil {
		return nil, fmt.Errorf("exchange: load markets: %w", err)
	}
	defer rows.Close()

	out := map[string]MarketCfg{}
	for rows.Next() {
		var m MarketCfg
		// Waiver: the SELECT keeps the spec-#21 storage column name
		// `symbol`; the Go field and the wire/glossary name are Market.
		if err := rows.Scan(&m.Market, &m.BaseCoin, &m.QuoteCoin, &m.BaseScale, &m.QuoteScale,
			&m.TickSize, &m.LotSize, &m.MakerFee, &m.TakerFee,
			&m.MinPrice, &m.MaxPrice, &m.MinAmount, &m.MaxAmount, &m.MinTurnover,
			&m.MarketOrders, &m.Enabled, &m.Visible, &m.Sort, &m.Zone, &m.Featured,
			&m.MaxOpen, &m.TTLSeconds); err != nil {
			return nil, fmt.Errorf("exchange: scan market: %w", err)
		}
		tick, err := rawAtScale(m.TickSize, m.QuoteScale)
		if err != nil || tick <= 0 {
			return nil, fmt.Errorf("exchange: market %s: bad tick_size %q", m.Market, m.TickSize)
		}
		lot, err := rawAtScale(m.LotSize, m.BaseScale)
		if err != nil || lot <= 0 {
			return nil, fmt.Errorf("exchange: market %s: bad lot_size %q", m.Market, m.LotSize)
		}
		m.TickRaw, m.LotRaw = tick, lot
		out[m.Market] = m
	}
	return out, rows.Err()
}

// rawAtScale converts a decimal string to its integer raw value at scale,
// with the engine's exact boundary semantics (sub-scale precision is an
// error: market config must be exact).
func rawAtScale(s string, scale int) (int64, error) {
	v, ok := engine.ParseRaw(s, scale)
	if !ok {
		return 0, fmt.Errorf("%q not representable at scale %d", s, scale)
	}
	return v, nil
}
