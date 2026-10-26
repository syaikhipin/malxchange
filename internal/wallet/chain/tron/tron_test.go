package tron

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/syaikhipin/malxchange/internal/wallet/chain"
	"github.com/syaikhipin/malxchange/internal/wallet/keys"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// stubFixture boots the in-process fake java-tron plus a Client with
// deterministic mining (auto-mine off; tests mine explicitly).
func stubFixture(t *testing.T) (*Client, *StubNode, *keys.Account) {
	t.Helper()
	seed, err := keys.Seed(testMnemonic, "")
	if err != nil {
		t.Fatal(err)
	}
	acct, err := keys.DeriveAccount(seed, keys.TRON, keys.MainNet)
	if err != nil {
		t.Fatal(err)
	}
	tumbler, err := acct.AddressAt(9)
	if err != nil {
		t.Fatal(err)
	}
	node := NewStub(StubConfig{
		Tumbler:       tumbler,
		TRXFund:       1_000_000_000_000, // 1,000,000 TRX
		Token:         DefaultStubToken,
		TokenDecimals: 6,
		TokenFund:     big.NewInt(10_000_000_000), // 10,000 USDT
	})
	node.SetAutoMine(false)
	if err := node.Start("127.0.0.1:0", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cl, err := New(ctx, node.GRPCAddr(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	cl.RegisterToken(DefaultStubToken, 6)
	return cl, node, acct
}

func findDeposit(deps []chain.Deposit, txid string) *chain.Deposit {
	for i := range deps {
		if deps[i].TxID == txid {
			return &deps[i]
		}
	}
	return nil
}

func TestScanFindsTRXAndTRC20Deposits(t *testing.T) {
	cl, node, acct := stubFixture(t)
	ctx := context.Background()

	deposit, err := acct.AddressAt(0)
	if err != nil {
		t.Fatal(err)
	}
	// Stage from the tumbler: 12.5 TRX and 25 USDT in one block.
	if err := node.Transfer(deposit, 12_500_000, big.NewInt(25_000_000)); err != nil {
		t.Fatal(err)
	}

	watch := map[string]string{deposit: deposit}
	tip, err := cl.Height(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tip != 1 {
		t.Fatalf("tip = %d, want 1", tip)
	}
	deps, spends, err := cl.ScanBlock(ctx, 1, watch)
	if err != nil {
		t.Fatal(err)
	}
	if len(spends) != 0 {
		t.Fatalf("spends = %v, want none on TRON", spends)
	}
	var (
		gotTRX, gotUSDT   bool
		trxVout, usdtVout int32
	)
	for _, d := range deps {
		if d.Address != deposit {
			t.Fatalf("deposit to %s, want %s", d.Address, deposit)
		}
		if d.Height != 1 {
			t.Fatalf("height = %d, want 1", d.Height)
		}
		switch {
		case d.Amount.Equal(decimal.NewFromFloat(12.5)):
			gotTRX = true
			trxVout = d.Vout
		case d.Amount.Equal(decimal.NewFromInt(25)):
			gotUSDT = true
			usdtVout = d.Vout
		}
	}
	if !gotTRX || !gotUSDT {
		t.Fatalf("deposits = %v, want 12.5 TRX and 25 USDT", deps)
	}
	if trxVout != 0 {
		t.Fatalf("TRX vout = %d, want 0", trxVout)
	}
	// The TRC-20 call is the first contract call of the block.
	if usdtVout != 0 {
		t.Fatalf("USDT vout = %d, want 0 (first contract call in block)", usdtVout)
	}
}

func TestTxExistsDepthDecimalsBalance(t *testing.T) {
	cl, node, acct := stubFixture(t)
	ctx := context.Background()

	deposit, err := acct.AddressAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Transfer(deposit, 3_000_000, big.NewInt(7_500_000)); err != nil {
		t.Fatal(err)
	}
	deps, _, err := cl.ScanBlock(ctx, 1, map[string]string{deposit: deposit})
	if err != nil || len(deps) != 2 {
		t.Fatalf("scan: %v %v", deps, err)
	}

	ok, err := cl.TxExists(ctx, deps[0].TxID, 1)
	if err != nil || !ok {
		t.Fatalf("TxExists(inclusion) = %v %v, want true", ok, err)
	}
	ok, _ = cl.TxExists(ctx, "deadbeef", 1)
	if ok {
		t.Fatal("TxExists(unknown) = true")
	}

	depth, err := cl.Depth(ctx, deps[0].TxID)
	if err != nil || depth != 1 {
		t.Fatalf("depth = %d %v, want 1", depth, err)
	}
	node.Mine(2)
	depth, err = cl.Depth(ctx, deps[0].TxID)
	if err != nil || depth != 3 {
		t.Fatalf("depth after 2 mines = %d %v, want 3", depth, err)
	}
	if d, _ := cl.Depth(ctx, "deadbeef"); d != -1 {
		t.Fatalf("depth(unknown) = %d, want -1", d)
	}

	if dp, _ := cl.Decimals(ctx, ""); dp != 6 {
		t.Fatalf("Decimals(TRX) = %d, want 6", dp)
	}
	if dp, err := cl.Decimals(ctx, DefaultStubToken); err != nil || dp != 6 {
		t.Fatalf("Decimals(USDT) = %d %v, want 6", dp, err)
	}

	trx, err := cl.TRXBalance(ctx, deposit)
	if err != nil || !trx.Equal(decimal.NewFromInt(3)) {
		t.Fatalf("TRXBalance = %s %v, want 3", trx, err)
	}
	usdt, err := cl.TRC20Balance(ctx, DefaultStubToken, deposit)
	if err != nil || !usdt.Equal(decimal.NewFromFloat(7.5)) {
		t.Fatalf("TRC20Balance = %s %v, want 7.5", usdt, err)
	}
}

func TestSignedSendRoundTrip(t *testing.T) {
	cl, node, acct := stubFixture(t)
	ctx := context.Background()

	deposit, err := acct.AddressAt(2)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := acct.AddressAt(3)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := acct.KeyAt(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Fee pre-funding: 10 TRX + 100 USDT lands on the deposit key.
	if err := node.Transfer(deposit, 10_000_000, big.NewInt(100_000_000)); err != nil {
		t.Fatal(err)
	}

	// Native send from the deposit key.
	trxTx, err := cl.TRXTransfer(ctx, priv, dest, decimal.NewFromFloat(2.5))
	if err != nil {
		t.Fatal(err)
	}
	node.Mine(1)
	deps, _, err := cl.ScanBlock(ctx, 2, map[string]string{dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	if d := findDeposit(deps, trxTx); d == nil || !d.Amount.Equal(decimal.NewFromFloat(2.5)) || d.Vout != 0 {
		t.Fatalf("TRX send not scanned: %v (txid %s)", deps, trxTx)
	}

	// TRC-20 send from the same key.
	tokTx, err := cl.TRC20Transfer(ctx, DefaultStubToken, priv, dest, decimal.NewFromInt(40), 6)
	if err != nil {
		t.Fatal(err)
	}
	node.Mine(1)
	deps, _, err = cl.ScanBlock(ctx, 3, map[string]string{dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	if d := findDeposit(deps, tokTx); d == nil || !d.Amount.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("TRC-20 send not scanned: %v (txid %s)", deps, tokTx)
	}

	// Exact final balances: deposit 7.5 TRX / 60 USDT, dest 2.5 / 40.
	dTrx, _ := cl.TRXBalance(ctx, deposit)
	dUsdt, _ := cl.TRC20Balance(ctx, DefaultStubToken, deposit)
	sTrx, _ := cl.TRXBalance(ctx, dest)
	sUsdt, _ := cl.TRC20Balance(ctx, DefaultStubToken, dest)
	if !dTrx.Equal(decimal.NewFromFloat(7.5)) || !dUsdt.Equal(decimal.NewFromInt(60)) ||
		!sTrx.Equal(decimal.NewFromFloat(2.5)) || !sUsdt.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("balances: deposit %s TRX / %s USDT, dest %s TRX / %s USDT", dTrx, dUsdt, sTrx, sUsdt)
	}

	// Overspending is rejected by the stub ledger.
	if _, err := cl.TRXTransfer(ctx, priv, dest, decimal.NewFromInt(1000)); err == nil {
		t.Fatal("overdraft TRXTransfer accepted")
	}
}

func TestAdminHTTP(t *testing.T) {
	cl, node, acct := stubFixture(t)
	ctx := context.Background()

	deposit, err := acct.AddressAt(4)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"to": "` + deposit + `", "trx": "1.25", "amount": "3.5"}`
	resp, err := httpPost("http://"+node.HTTPAddr()+"/stub/transfer", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("transfer status = %d", resp.StatusCode)
	}
	mineResp, err := httpPost("http://"+node.HTTPAddr()+"/stub/mine?n=1", "")
	if err != nil {
		t.Fatal(err)
	}
	mineResp.Body.Close()

	bal, err := cl.TRC20Balance(ctx, DefaultStubToken, deposit)
	if err != nil || !bal.Equal(decimal.NewFromFloat(3.5)) {
		t.Fatalf("admin transfer balance = %s %v, want 3.5", bal, err)
	}
}

func httpPost(url, body string) (*http.Response, error) {
	return http.Post(url, "application/json", strings.NewReader(body))
}

func TestParseTRC20TransferSkipsZeroAmount(t *testing.T) {
	// A zero-value TRC-20 transfer aimed at a watched address must not
	// decode into a Deposit: the deposits CHECK (amount > 0) would wedge
	// the watcher on the block forever (audit issue #81).
	data := make([]byte, 68)
	copy(data, trc20TransferMethod)

	if _, _, ok := parseTRC20Transfer(data); ok {
		t.Fatal("zero-value TRC-20 transfer must be rejected")
	}

	data[67] = 1
	to, amt, ok := parseTRC20Transfer(data)
	if !ok {
		t.Fatal("positive TRC-20 transfer must parse")
	}
	if to == "" || !amt.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("to = %q, amount = %s, want a non-empty address and 1", to, amt)
	}
}
