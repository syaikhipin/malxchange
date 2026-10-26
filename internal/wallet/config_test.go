package wallet

import (
	"sync"
	"testing"
)

// Chain TLS verification must fail closed (audit issue #85): only the
// explicit "true" opts out — for the in-compose nodes' self-signed
// certificates — and every other value, including unset, verifies. On
// main the flag failed open: anything except the literal "false"
// skipped verification on the RPC that drives deposit crediting.
func TestChainTLSSkipVerifyFailsClosed(t *testing.T) {
	cases := map[string]bool{
		"":      false, // unset — verify (the fix; main skipped here)
		"true":  true,  // explicit opt-out: in-compose self-signed certs
		"false": false,
		"yes":   false, // unrecognized — verify, never skip
		"TRUE":  false, // exact match only
	}
	for val, want := range cases {
		t.Setenv("MAL_CHAIN_TLS_SKIP_VERIFY", val)
		envOnce = sync.Once{} // reset the cached environment
		envVal = WalletEnv{}
		if got := LoadWalletEnv().ChainTLSSkip; got != want {
			t.Errorf("MAL_CHAIN_TLS_SKIP_VERIFY=%q: ChainTLSSkip = %v, want %v", val, got, want)
		}
	}
}
