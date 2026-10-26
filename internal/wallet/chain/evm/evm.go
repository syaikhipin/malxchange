// Package evm is the ETH/ERC-20 client over a JSON-RPC endpoint
// (own anvil/geth or third-party RPC, ADR-0008): watcher reads and
// local signing.
package evm

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"

	"github.com/syaikhipin/malxchange/internal/wallet/chain"
)

// NativeDecimals is the dp of the base coin (wei).
const NativeDecimals int32 = 18

// transferTopic is the keccak of the ERC-20 Transfer event signature.
var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// erc20ABI carries just the views/sender we drive by hand; no codegen.
var erc20ABI = mustABI(`[
	{"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"stateMutability":"view","type":"function"},
	{"constant":true,"inputs":[{"name":"","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
	{"constant":false,"inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"stateMutability":"nonpayable","type":"function"}
]`)

func mustABI(j string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(j))
	if err != nil {
		panic("evm: bad embedded ABI: " + err.Error())
	}
	return a
}

// weiToDecimal converts a base-coin integer to an exact 18dp decimal.
func weiToDecimal(wei *big.Int) decimal.Decimal {
	return decimal.NewFromBigInt(wei, -NativeDecimals)
}

// toUnits converts a coin/token decimal to integer minor units at dp.
func toUnits(amount decimal.Decimal, dp int32) *big.Int {
	return amount.Shift(dp).BigInt()
}

// Client is a JSON-RPC EVM client with an optional fallback endpoint.
// It implements chain.Watcher for deposit scanning and exposes the
// local-signing senders the sweeper uses. Nonces are sequential per
// address and serialized under sendMu across the whole client: each
// send fetches NonceAt(head), signs and broadcasts while holding the
// mutex, so concurrent sends from one address can never reuse a
// nonce. We read the nonce at head rather than pending because our
// devnets (anvil) automine — pending == head — and against a public
// RPC head avoids the classic overcount when a pending-pool tx is
// evicted.
type Client struct {
	primary   *ethclient.Client
	fallback  *ethclient.Client
	chainID   *big.Int
	sendMu    sync.Mutex // serializes nonce-fetch→sign→broadcast
	tokensMu  sync.RWMutex
	tokens    map[string]int32 // lowercase contract hex -> decimals
	closeOnce sync.Once
}

// New dials rpcURL (and the optional fallbackURL, "" to skip) and
// pins the chain ID used for signing.
func New(ctx context.Context, rpcURL, fallbackURL string) (*Client, error) {
	primary, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	c := &Client{primary: primary, tokens: map[string]int32{}}
	if fallbackURL != "" {
		fb, ferr := ethclient.DialContext(ctx, fallbackURL)
		if ferr != nil {
			primary.Close()
			return nil, ferr
		}
		c.fallback = fb
	}
	cid, err := primary.ChainID(ctx)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.chainID = cid
	return c, nil
}

// rpcErr is rpc for calls that return nothing but an error.
func rpcErr(c *Client, fn func(*ethclient.Client) error) error {
	_, err := rpc(c, func(cl *ethclient.Client) (struct{}, error) {
		return struct{}{}, fn(cl)
	})
	return err
}

// Close releases both RPC connections.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.primary.Close()
		if c.fallback != nil {
			c.fallback.Close()
		}
	})
}

// rpc runs fn against the primary endpoint, retrying once on the
// fallback when the primary errors.
func rpc[T any](c *Client, fn func(*ethclient.Client) (T, error)) (T, error) {
	v, err := fn(c.primary)
	if err == nil || c.fallback == nil {
		return v, err
	}
	return fn(c.fallback)
}

// RegisterToken adds a token contract to the scan registry; the
// wallet does this at boot from Coin config.
func (c *Client) RegisterToken(contract string, decimals int32) {
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	c.tokens[strings.ToLower(contract)] = decimals
}

// tokenRegistry returns a copy of the registry plus the address set
// for log filtering.
func (c *Client) tokenRegistry() (map[string]int32, []common.Address) {
	c.tokensMu.RLock()
	defer c.tokensMu.RUnlock()
	cp := make(map[string]int32, len(c.tokens))
	addrs := make([]common.Address, 0, len(c.tokens))
	for hex, dp := range c.tokens {
		cp[hex] = dp
		addrs = append(addrs, common.HexToAddress(hex))
	}
	return cp, addrs
}

// Height is the current chain tip.
func (c *Client) Height(ctx context.Context) (int64, error) {
	h, err := rpc(c, func(cl *ethclient.Client) (*types.Header, error) {
		return cl.HeaderByNumber(ctx, nil)
	})
	if err != nil {
		return 0, err
	}
	return h.Number.Int64(), nil
}

// compile-time interface check.
var _ chain.Watcher = (*Client)(nil)

// ScanBlock returns native-value deposits and ERC-20 Transfer logs
// paying watched addresses in [height]. EVM has no spend outputs, so
// the second return is always nil. Addresses match
// case-insensitively (common.HexToAddress normalization).
func (c *Client) ScanBlock(ctx context.Context, height int64, watch map[string]string) ([]chain.Deposit, []chain.Spend, error) {
	if len(watch) == 0 {
		return nil, nil, nil
	}
	lower := make(map[string]string, len(watch)) // lower hex -> reported address
	for addr := range watch {
		lower[strings.ToLower(addr)] = addr
	}

	blk, err := rpc(c, func(cl *ethclient.Client) (*types.Block, error) {
		return cl.BlockByNumber(ctx, big.NewInt(height))
	})
	if err != nil {
		return nil, nil, err
	}

	var deps []chain.Deposit
	for _, tx := range blk.Transactions() {
		to := tx.To()
		if to == nil || tx.Value().Sign() <= 0 {
			continue
		}
		if addr, ok := lower[strings.ToLower(to.String())]; ok {
			deps = append(deps, chain.Deposit{
				TxID:    tx.Hash().Hex(),
				Vout:    0,
				Address: addr,
				Amount:  weiToDecimal(tx.Value()),
				Height:  height,
			})
		}
	}

	tokens, contracts := c.tokenRegistry()
	if len(contracts) > 0 {
		logs, err := rpc(c, func(cl *ethclient.Client) ([]types.Log, error) {
			return cl.FilterLogs(ctx, ethereum.FilterQuery{
				FromBlock: big.NewInt(height),
				ToBlock:   big.NewInt(height),
				Addresses: contracts,
			})
		})
		if err != nil {
			return nil, nil, err
		}
		for _, lg := range logs {
			dp, ok := tokens[strings.ToLower(lg.Address.String())]
			if !ok { // contract deregistered mid-scan; skip
				continue
			}
			if d, hit := parseTransferLog(lg, lower, dp, height); hit {
				deps = append(deps, d)
			}
		}
	}
	return deps, nil, nil
}

// parseTransferLog turns an ERC-20 Transfer log into a Deposit when
// the recipient is watched. The value lives in the 32-byte data word.
func parseTransferLog(lg types.Log, watch map[string]string, dp int32, height int64) (chain.Deposit, bool) {
	if len(lg.Topics) != 3 || lg.Topics[0] != transferTopic || len(lg.Data) != 32 {
		return chain.Deposit{}, false
	}
	to := common.BytesToAddress(lg.Topics[2].Bytes())
	addr, ok := watch[strings.ToLower(to.String())]
	if !ok {
		return chain.Deposit{}, false
	}
	amt := new(big.Int).SetBytes(lg.Data)
	if amt.Sign() <= 0 {
		// A zero-value Transfer can be sent by anyone to any address.
		// Recorded, it would violate the deposits CHECK (amount > 0),
		// fail the watcher's transaction before the cursor saves, and
		// re-scan the same block forever — wedging credits for every
		// member of this Coin (audit issue #81).
		return chain.Deposit{}, false
	}
	return chain.Deposit{
		TxID:    lg.TxHash.Hex(),
		Vout:    int32(lg.Index),
		Address: addr,
		Amount:  decimal.NewFromBigInt(amt, -dp),
		Height:  height,
	}, true
}
func (c *Client) TxExists(ctx context.Context, txid string, height int64) (bool, error) {
	hash := common.HexToHash(txid)
	blk, err := rpc(c, func(cl *ethclient.Client) (*types.Block, error) {
		return cl.BlockByNumber(ctx, big.NewInt(height))
	})
	if err != nil {
		return false, err
	}
	for _, tx := range blk.Transactions() {
		if tx.Hash() == hash {
			return true, nil
		}
	}
	r, err := rpc(c, func(cl *ethclient.Client) (*types.Receipt, error) {
		return cl.TransactionReceipt(ctx, hash)
	})
	if err != nil || r == nil { // not mined under the current chain
		return false, nil
	}
	return r.BlockNumber.Int64() == height, nil
}

// Depth is tip - inclusion_height + 1; -1 when the chain no longer
// knows the tx (no receipt is not an error).
func (c *Client) Depth(ctx context.Context, txid string) (int64, error) {
	r, err := rpc(c, func(cl *ethclient.Client) (*types.Receipt, error) {
		return cl.TransactionReceipt(ctx, common.HexToHash(txid))
	})
	if err != nil || r == nil {
		return -1, nil
	}
	tip, err := c.Height(ctx)
	if err != nil {
		return -1, err
	}
	return tip - r.BlockNumber.Int64() + 1, nil
}

// Decimals returns 18 for the native coin (contract ""), otherwise
// the token's on-chain decimals().
func (c *Client) Decimals(ctx context.Context, contract string) (int32, error) {
	if contract == "" {
		return NativeDecimals, nil
	}
	tokens, _ := c.tokenRegistry()
	if dp, ok := tokens[strings.ToLower(contract)]; ok {
		return dp, nil
	}
	return c.callDecimals(ctx, common.HexToAddress(contract))
}

// callContract runs a read-only ABI call and returns raw return data.
func (c *Client) callContract(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	msg := ethereum.CallMsg{To: &to, Data: data}
	return rpc(c, func(cl *ethclient.Client) ([]byte, error) {
		return cl.CallContract(ctx, msg, nil)
	})
}

// decimalsFor resolves a token's dp: registry first, then on-chain.
func (c *Client) decimalsFor(ctx context.Context, token common.Address) (int32, error) {
	tokens, _ := c.tokenRegistry()
	if dp, ok := tokens[strings.ToLower(token.String())]; ok {
		return dp, nil
	}
	return c.callDecimals(ctx, token)
}

func (c *Client) callDecimals(ctx context.Context, token common.Address) (int32, error) {
	data, err := erc20ABI.Pack("decimals")
	if err != nil {
		return 0, err
	}
	out, err := c.callContract(ctx, token, data)
	if err != nil {
		return 0, err
	}
	vals, err := erc20ABI.Unpack("decimals", out)
	if err != nil {
		return 0, err
	}
	if len(vals) != 1 {
		return 0, errors.New("evm: decimals() returned no value")
	}
	dp, ok := vals[0].(uint8)
	if !ok {
		return 0, errors.New("evm: decimals() returned unexpected type")
	}
	return int32(dp), nil
}
