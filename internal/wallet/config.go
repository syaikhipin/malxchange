package wallet

import (
	"bufio"
	"os"
	"strings"
	"sync"

	"github.com/syaikhipin/malxchange/internal/config"
	"github.com/syaikhipin/malxchange/internal/wallet/keys"
)

// WalletEnv is the wallet service's own MAL_WALLET_* / chain-RPC
// environment, loaded once at boot (the shared internal/config covers
// Postgres/Kafka/HTTP).
type WalletEnv struct {
	Net            string // keys.MainNet | keys.SimNet (address encoding)
	KeystorePath   string // encrypted hot keystore; "" = watch-only
	Passphrase     string // env-file passphrase (MAL_HOT_KEYSTORE_PASSPHRASE)
	PollInterval   string // watcher cadence (time.ParseDuration)
	BtcRPCURL      string
	BtcRPCUser     string
	BtcRPCPass     string
	LtcRPCURL      string
	LtcRPCUser     string
	LtcRPCPass     string
	EvmRPCURL      string
	EvmFallbackURL string
	TronEndpoint   string
	TronAPIKey     string
	ChainTLSSkip   bool   // opt-in skip for self-signed node certs ("true"); verification is the default (audit issue #85)
	TronFundBuffer string // TRX kept on the hot wallet for fee burning
}

var (
	envOnce sync.Once
	envVal  WalletEnv
)

// LoadWalletEnv reads the wallet environment (idempotent).
func LoadWalletEnv() WalletEnv {
	envOnce.Do(func() {
		envVal = WalletEnv{
			Net:            config.EnvDefault("MAL_WALLET_NET", keys.MainNet),
			KeystorePath:   os.Getenv("MAL_WALLET_KEYSTORE"),
			Passphrase:     os.Getenv("MAL_HOT_KEYSTORE_PASSPHRASE"),
			PollInterval:   config.EnvDefault("MAL_WALLET_POLL", "2s"),
			BtcRPCURL:      os.Getenv("MAL_BTCD_URL"),
			BtcRPCUser:     config.EnvDefault("MAL_BTCD_RPCUSER", "malxchange"),
			BtcRPCPass:     os.Getenv("MAL_BTCD_RPCPASSWORD"),
			LtcRPCURL:      os.Getenv("MAL_LTCD_URL"),
			LtcRPCUser:     config.EnvDefault("MAL_LTCD_RPCUSER", "malxchange"),
			LtcRPCPass:     os.Getenv("MAL_LTCD_RPCPASSWORD"),
			EvmRPCURL:      os.Getenv("MAL_EVM_RPC_URL"),
			EvmFallbackURL: os.Getenv("MAL_EVM_RPC_FALLBACK_URL"),
			TronEndpoint:   os.Getenv("MAL_TRON_ENDPOINT"),
			TronAPIKey:     os.Getenv("MAL_TRON_API_KEY"),
			ChainTLSSkip:   os.Getenv("MAL_CHAIN_TLS_SKIP_VERIFY") == "true",
		}
	})
	return envVal
}

// WalletNet is the address-encoding network (mainnet unless the test
// profile sets simnet).
func WalletNet() string { return LoadWalletEnv().Net }

// readSecretLine reads a single line from a file (passphrase-file
// support for dev/test; production uses the env var).
func readSecretLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}
