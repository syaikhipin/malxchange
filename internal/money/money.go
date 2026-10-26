// Package money implements the malxchange money conventions (ADR-0005):
//
//   - Postgres NUMERIC columns, shopspring/decimal domain values
//   - decimal strings on every API surface (REST, WebSocket, Kafka)
//   - floor rounding (truncate toward zero) on every derived amount
//   - per-Coin Scale and per-Market tick size / lot size conformity
//
// A decimal string is ^-?\d+(\.\d+)?$ — no exponent, no sign character
// other than a leading '-', no trailing dot. That is the only wire form.
package money

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

var plainString = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// Parse parses a decimal string exactly as defined by the package contract.
// It rejects exponents, empty strings, "+", spaces, and trailing dots.
func Parse(s string) (decimal.Decimal, error) {
	if !plainString.MatchString(s) {
		return decimal.Decimal{}, fmt.Errorf("money: %q is not a decimal string", s)
	}
	return decimal.NewFromString(s)
}

// MustParse is Parse for constant inputs; it panics on invalid input.
func MustParse(s string) decimal.Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

// FloorTo truncates d toward zero at the given scale (dp). Rounding can
// never create money; dust stays with the member (ADR-0005).
func FloorTo(d decimal.Decimal, scale int32) decimal.Decimal {
	return d.Truncate(scale)
}

// Format floors d at scale and renders it as a decimal string with exactly
// `scale` decimal places — the canonical serialization for APIs.
func Format(d decimal.Decimal, scale int32) string {
	return FloorTo(d, scale).StringFixed(scale)
}

// WithinScale reports whether d is exact at scale (no digits beyond it).
func WithinScale(d decimal.Decimal, scale int32) bool {
	return d.Equal(d.Truncate(scale))
}

// Conforms reports whether d is a whole multiple of quantum (a Market's
// tick size for prices, lot size for amounts — admission rejects
// non-multiples per ADR-0009).
func Conforms(d, quantum decimal.Decimal) bool {
	if quantum.IsZero() {
		return false
	}
	// Conforming means d is a WHOLE multiple of quantum: 123.45 with tick
	// 0.1 divides exactly to 1234.5 — remainder zero, still non-conforming.
	q, _ := d.QuoRem(quantum, divPrecision)
	return q.IsInteger()
}

// Ticks converts d to an integer number of quanta (the engine's in-memory
// book runs on int64 tick counts). The caller must have validated
// conformity; Ticks returns an error if d is not a whole multiple, so a
// missed validation cannot silently corrupt the book.
func Ticks(d, quantum decimal.Decimal) (int64, error) {
	if !Conforms(d, quantum) {
		return 0, fmt.Errorf("money: %s is not a whole multiple of %s", d, quantum)
	}
	q := d.DivRound(quantum, divPrecision)
	if !q.IsInteger() {
		return 0, fmt.Errorf("money: %s quanta is not an integer", q)
	}
	if q.Cmp(maxInt64Decimal) > 0 || q.Cmp(minInt64Decimal) < 0 {
		return 0, fmt.Errorf("money: %s quanta overflows int64", q)
	}
	return q.IntPart(), nil
}

// FromTicks renders an integer tick count back to a decimal amount.
func FromTicks(ticks int64, quantum decimal.Decimal) decimal.Decimal {
	return quantum.Mul(decimal.NewFromInt(ticks))
}

// Fee computes rate × base floored at scale — the per-fill fee rule of
// ADR-0009: charged in the received Coin, floored to that Coin's scale,
// dust (a fee flooring to zero) stays with the member.
func Fee(base, rate decimal.Decimal, scale int32) decimal.Decimal {
	return FloorTo(base.Mul(rate), scale)
}

// TrimScale renders d with trailing zeros removed but never changes value.
func TrimScale(d decimal.Decimal) string {
	return strings.TrimRight(strings.TrimRight(d.String(), "0"), ".")
}

// int64 bounds as decimals, for Ticks' overflow guard.
var (
	maxInt64Decimal = decimal.NewFromInt(math.MaxInt64)
	minInt64Decimal = decimal.NewFromInt(math.MinInt64)
)

// divPrecision is the division precision used for conformity checks; tick
// and lot sizes are short decimal strings, so 32 dp is far beyond exact.
const divPrecision = 32
