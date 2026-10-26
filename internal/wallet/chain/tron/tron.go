// Package tron is the TRC-20 client over java-tron gRPC (ADR-0008: no
// maintained Go node exists) plus the local signing path for
// TRX/TRC-20 sends.
//
// Addressing: TRON addresses are base58check strings (T..., the
// 0x41-prefixed 21-byte payload) everywhere at this seam.
//
// Deposit identity per spec #22: a native TRX transfer is always
// contract output 0 of its transaction (Vout = 0); a TRC-20 transfer
// is a contract call inside a transaction, so its Vout is the ordinal
// of the smart-contract call within the block (0 for the first
// TriggerSmartContract in the block, 1 for the second, ...). Amounts
// are exact decimals: TRX in whole coins (sun = 1e-6 TRX), tokens in
// whole units at the token's on-chain decimals.
package tron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/client/transaction"
	"github.com/fbsobreira/gotron-sdk/pkg/common"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/syaikhipin/malxchange/internal/wallet/chain"
)

// sunDivisor scales sun (1e-6) to whole TRX.
var sunDivisor = decimal.New(1, 6)

// trc20TransferMethod is the ERC-20/TRC-20 transfer(address,uint256)
// selector; transferFrom and friends are not deposit-relevant.
var trc20TransferMethod = []byte{0xa9, 0x05, 0x9c, 0xbb}

// depthWindow bounds the backward block scan of Depth (tip-200).
const depthWindow = 200

// Client is the TRON/TRC-20 chain client: a chain.Watcher plus the
// signing senders. It is safe for concurrent use.
type Client struct {
	g *client.GrpcClient

	mu     sync.RWMutex
	tokens map[string]int32 // contract base58 -> decimals
}

// compile-time seam check.
var _ chain.Watcher = (*Client)(nil)

// New connects to a java-tron gRPC endpoint (host:port). apiKey may be
// empty (self-hosted nodes and the test stub need none).
func New(ctx context.Context, endpoint, apiKey string) (*Client, error) {
	g := client.NewGrpcClient(endpoint)
	if apiKey != "" {
		_ = g.SetAPIKey(apiKey)
	}
	if err := g.Start(grpc.WithTransportCredentials(insecure.NewCredentials())); err != nil {
		return nil, fmt.Errorf("tron: dial %s: %w", endpoint, err)
	}
	_ = g.SetContext(ctx)
	if _, err := g.GetNowBlockCtx(ctx); err != nil {
		g.Stop()
		return nil, fmt.Errorf("tron: endpoint handshake: %w", err)
	}
	return &Client{g: g, tokens: map[string]int32{}}, nil
}

// Close tears down the gRPC connection.
func (c *Client) Close() { c.g.Stop() }

// RegisterToken whitelists a TRC-20 contract for ScanBlock matching.
// Deposit scans only report transfers of registered contracts.
func (c *Client) RegisterToken(contract string, decimals int32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[strings.TrimSpace(contract)] = decimals
}

// Height is the current chain tip.
func (c *Client) Height(ctx context.Context) (int64, error) {
	b, err := c.g.GetNowBlockCtx(ctx)
	if err != nil {
		return 0, err
	}
	return b.GetBlockHeader().GetRawData().GetNumber(), nil
}

// ScanBlock returns deposits paying watched addresses in the block at
// height: native TransferContract sends and TRC-20 transfer calls on
// registered contracts. Spends are unused on TRON (account model) and
// always nil.
func (c *Client) ScanBlock(ctx context.Context, height int64, watch map[string]string) ([]chain.Deposit, []chain.Spend, error) {
	if len(watch) == 0 {
		return nil, nil, nil
	}
	be, err := c.g.GetBlockByNumCtx(ctx, height)
	if err != nil {
		return nil, nil, fmt.Errorf("tron: block %d: %w", height, err)
	}
	c.mu.RLock()
	tokens := make(map[string]int32, len(c.tokens))
	for k, v := range c.tokens {
		tokens[k] = v
	}
	c.mu.RUnlock()

	var deps []chain.Deposit
	callOrd := int32(0) // TRC-20 Vout: smart-contract-call ordinal in the block
	for _, txe := range be.GetTransactions() {
		tx := txe.GetTransaction()
		txid := txIDHex(txe.GetTxid(), tx)
		for _, ct := range tx.GetRawData().GetContract() {
			switch ct.GetType() {
			case core.Transaction_Contract_TransferContract:
				tc := new(core.TransferContract)
				if err := ct.GetParameter().UnmarshalTo(tc); err != nil {
					continue
				}
				to := common.EncodeCheck(tc.GetToAddress())
				if tc.GetAmount() <= 0 {
					continue // defense in depth: a Deposit needs amount > 0 (audit issue #81)
				}
				if _, ok := watch[to]; ok {
					deps = append(deps, chain.Deposit{
						TxID:    txid,
						Vout:    0, // the single output of a native transfer
						Address: to,
						Amount:  decimal.NewFromInt(tc.GetAmount()).Div(sunDivisor),
						Height:  height,
					})
				}
			case core.Transaction_Contract_TriggerSmartContract:
				sc := new(core.TriggerSmartContract)
				if err := ct.GetParameter().UnmarshalTo(sc); err != nil {
					callOrd++
					continue
				}
				ord := callOrd
				callOrd++
				contract := common.EncodeCheck(sc.GetContractAddress())
				if _, ok := tokens[contract]; !ok {
					continue
				}
				to, amount, ok := parseTRC20Transfer(sc.GetData())
				if !ok {
					continue
				}
				if _, ok := watch[to]; ok {
					deps = append(deps, chain.Deposit{
						TxID:    txid,
						Vout:    ord, // contract-call ordinal within the block
						Address: to,
						Amount:  amount.Div(decimal.New(1, tokens[contract])),
						Height:  height,
					})
				}
			}
		}
	}
	return deps, nil, nil
}

// TxExists reports whether txid is embedded in the block at height.
func (c *Client) TxExists(ctx context.Context, txid string, height int64) (bool, error) {
	be, err := c.g.GetBlockByNumCtx(ctx, height)
	if err != nil {
		return false, err
	}
	for _, txe := range be.GetTransactions() {
		if txIDHex(txe.GetTxid(), txe.GetTransaction()) == txid {
			return true, nil
		}
	}
	return false, nil
}

// Depth is tip - inclusion_height + 1, scanning at most depthWindow
// blocks back from the tip; -1 when the tx is not found in the window.
func (c *Client) Depth(ctx context.Context, txid string) (int64, error) {
	tip, err := c.Height(ctx)
	if err != nil {
		return -1, err
	}
	low := tip - depthWindow
	if low < 0 {
		low = 0
	}
	for h := tip; h >= low; h-- {
		ok, err := c.TxExists(ctx, txid, h)
		if err != nil {
			return -1, err
		}
		if ok {
			return tip - h + 1, nil
		}
	}
	return -1, nil
}

// Decimals returns the on-chain dp of the native coin (contract empty:
// TRX has 6 sun dp) or of the token at contract.
func (c *Client) Decimals(ctx context.Context, contract string) (int32, error) {
	if contract == "" {
		return 6, nil
	}
	d, err := c.g.TRC20GetDecimalsCtx(ctx, contract)
	if err != nil {
		return 0, fmt.Errorf("tron: decimals(%s): %w", contract, err)
	}
	return int32(d.Int64()), nil
}

// TRXTransfer signs and broadcasts a native TRX send from privKey.
func (c *Client) TRXTransfer(ctx context.Context, privKey []byte, to string, amount decimal.Decimal) (string, error) {
	sun := toBaseUnits(amount, 6)
	if sun.Sign() <= 0 {
		return "", fmt.Errorf("tron: non-positive TRX amount %s", amount)
	}
	from, err := addressOfKey(privKey)
	if err != nil {
		return "", err
	}
	owner, err := common.DecodeCheck(from)
	if err != nil {
		return "", err
	}
	dest, err := common.DecodeCheck(to)
	if err != nil {
		return "", fmt.Errorf("tron: bad to address %q: %w", to, err)
	}
	param, err := anypb.New(&core.TransferContract{OwnerAddress: owner, ToAddress: dest, Amount: sun.Int64()})
	if err != nil {
		return "", err
	}
	return c.signAndBroadcast(ctx, privKey, &core.Transaction_Contract{
		Type:      core.Transaction_Contract_TransferContract,
		Parameter: param,
	}, 0)
}

// TRC20Transfer signs and broadcasts a TRC-20 transfer(address,uint256)
// call on contract from privKey.
func (c *Client) TRC20Transfer(ctx context.Context, contract string, privKey []byte, to string, amount decimal.Decimal, decimals int32) (string, error) {
	units := toBaseUnits(amount, decimals)
	if units.Sign() <= 0 {
		return "", fmt.Errorf("tron: non-positive token amount %s", amount)
	}
	from, err := addressOfKey(privKey)
	if err != nil {
		return "", err
	}
	owner, err := common.DecodeCheck(from)
	if err != nil {
		return "", err
	}
	dest, err := common.DecodeCheck(to)
	if err != nil {
		return "", fmt.Errorf("tron: bad to address %q: %w", to, err)
	}
	if len(dest) != 21 {
		return "", fmt.Errorf("tron: bad to address %q", to)
	}
	contractAddr, err := common.DecodeCheck(contract)
	if err != nil {
		return "", fmt.Errorf("tron: bad contract %q: %w", contract, err)
	}
	data := make([]byte, 0, 68)
	data = append(data, trc20TransferMethod...)
	data = append(data, pad32(dest[1:])...) // EVM-style 20-byte address
	var amt [32]byte
	units.FillBytes(amt[:])
	data = append(data, amt[:]...)
	param, err := anypb.New(&core.TriggerSmartContract{
		OwnerAddress:    owner,
		ContractAddress: contractAddr,
		Data:            data,
	})
	if err != nil {
		return "", err
	}
	return c.signAndBroadcast(ctx, privKey, &core.Transaction_Contract{
		Type:      core.Transaction_Contract_TriggerSmartContract,
		Parameter: param,
	}, 0)
}

// TRXBalance is the spendable TRX balance of addr.
func (c *Client) TRXBalance(ctx context.Context, addr string) (decimal.Decimal, error) {
	acc, err := c.g.GetAccountCtx(ctx, addr)
	if err != nil {
		return decimal.Zero, fmt.Errorf("tron: account %s: %w", addr, err)
	}
	return decimal.NewFromInt(acc.GetBalance()).Div(sunDivisor), nil
}

// TRC20Balance is the token balance of addr at contract.
func (c *Client) TRC20Balance(ctx context.Context, contract, addr string) (decimal.Decimal, error) {
	raw, err := common.DecodeCheck(addr)
	if err != nil || len(raw) != 21 {
		return decimal.Zero, fmt.Errorf("tron: bad address %q", addr)
	}
	data := "0x70a08231" + hex.EncodeToString(pad32(raw[1:])) // balanceOf(address)
	res, err := c.g.TRC20CallCtx(ctx, addr, contract, data, true, 0)
	if err != nil {
		return decimal.Zero, fmt.Errorf("tron: balanceOf(%s): %w", addr, err)
	}
	if len(res.GetConstantResult()) == 0 {
		return decimal.Zero, fmt.Errorf("tron: balanceOf(%s): empty result", addr)
	}
	dp, err := c.Decimals(ctx, contract)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromBigInt(new(big.Int).SetBytes(res.GetConstantResult()[0]), -dp), nil
}

// signAndBroadcast wraps the contract in a transaction referencing the
// current tip, signs it locally with the raw secp256k1 key and
// broadcasts; it returns the hex txid (sha256 of the unsigned raw
// proto, the TRON canonical id).
func (c *Client) signAndBroadcast(ctx context.Context, privKey []byte, contract *core.Transaction_Contract, feeLimit int64) (string, error) {
	tip, err := c.g.GetNowBlockCtx(ctx)
	if err != nil {
		return "", err
	}
	num := tip.GetBlockHeader().GetRawData().GetNumber()
	refHash := tip.GetBlockid()
	if len(refHash) > 8 {
		refHash = refHash[:8]
	}
	now := time.Now().UnixMilli()
	raw := &core.TransactionRaw{
		RefBlockBytes: []byte{byte(num >> 8), byte(num)},
		RefBlockNum:   num,
		RefBlockHash:  refHash,
		Expiration:    now + int64(10*time.Hour/time.Millisecond),
		Contract:      []*core.Transaction_Contract{contract},
		Timestamp:     now,
		FeeLimit:      feeLimit,
	}
	tx := &core.Transaction{RawData: raw}
	id := TxID(tx)

	key, err := crypto.ToECDSA(privKey)
	if err != nil {
		return "", fmt.Errorf("tron: private key: %w", err)
	}
	if _, err := transaction.SignTransactionECDSA(tx, key); err != nil {
		return "", fmt.Errorf("tron: sign: %w", err)
	}
	if _, err := c.g.BroadcastCtx(ctx, tx); err != nil {
		return "", fmt.Errorf("tron: broadcast: %w", err)
	}
	return id, nil
}

// TxID is the canonical TRON transaction id: sha256 of the marshalled
// raw proto WITHOUT the signature.
func TxID(tx *core.Transaction) string {
	raw, err := proto.Marshal(tx.GetRawData())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// parseTRC20Transfer extracts (to, amount) from a 68-byte
// transfer(address,uint256) calldata. ok is false for any other shape.
func parseTRC20Transfer(data []byte) (string, decimal.Decimal, bool) {
	if len(data) != 68 || string(data[:4]) != string(trc20TransferMethod) {
		return "", decimal.Zero, false
	}
	to := make([]byte, 0, 21)
	to = append(to, 0x41)
	to = append(to, data[16:36]...) // right-aligned 20-byte address
	amount := new(big.Int).SetBytes(data[36:68])
	if amount.Sign() <= 0 {
		// Zero-value TRC-20 transfers are a cheap spam vector aimed at
		// watched deposit addresses; the deposits CHECK (amount > 0)
		// would wedge the watcher on the block forever (audit issue #81).
		return "", decimal.Zero, false
	}
	return common.EncodeCheck(to), decimal.NewFromBigInt(amount, 0), true
}

func pad32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func addressOfKey(privKey []byte) (string, error) {
	key, err := crypto.ToECDSA(privKey)
	if err != nil {
		return "", fmt.Errorf("tron: private key: %w", err)
	}
	pub := crypto.CompressPubkey(&key.PublicKey)
	return tronAddressOfPubkey(pub)
}

// tronAddressOfPubkey mirrors keys.TronAddress so the client does not
// depend on the custody package.
func tronAddressOfPubkey(compressed []byte) (string, error) {
	key, err := crypto.DecompressPubkey(compressed)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	key.X.FillBytes(raw[:32])
	key.Y.FillBytes(raw[32:])
	h := crypto.Keccak256(raw)[12:]
	payload := append([]byte{0x41}, h...)
	return common.EncodeCheck(payload), nil
}

func toBaseUnits(amount decimal.Decimal, dp int32) *big.Int {
	return amount.Shift(dp).BigInt()
}

// txIDHex prefers the node-computed id and falls back to computing it
// from the raw proto.
func txIDHex(nodeID []byte, tx *core.Transaction) string {
	if len(nodeID) == 32 {
		return hex.EncodeToString(nodeID)
	}
	return TxID(tx)
}
