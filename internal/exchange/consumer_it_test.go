//go:build integration

// Engine-loop integration tests (spec #21): real Postgres + the in-process
// Kafka broker (kfake). Covers the ADR-0010 discipline end to end:
// fold+persist in one tx, offsets after commit, crash-redelivery dedupe,
// and recovery rebuilding an identical book.
package exchange

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/syaikhipin/malxchange/internal/config"
	"github.com/syaikhipin/malxchange/internal/events"
	"github.com/syaikhipin/malxchange/internal/kafka"
	"github.com/syaikhipin/malxchange/internal/migrations"
	"github.com/syaikhipin/malxchange/internal/money"
	"github.com/syaikhipin/malxchange/internal/outbox"
	"github.com/syaikhipin/malxchange/internal/pgmig"
)

var (
	itPGHost = os.Getenv("MAL_TEST_PG_HOST")
	itPGPW   = os.Getenv("MAL_TEST_PG_SUPERUSER_PASSWORD")
)

func TestMain(m *testing.M) {
	if itPGHost == "" || itPGPW == "" {
		fmt.Println("skipping exchange integration tests: set MAL_TEST_PG_* (make test-integration)")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// mintITDB mints a uniquely named throwaway database on the dev Postgres,
// creates it, and registers its FORCE-drop in t.Cleanup. The name is
// random per binary: sibling tickets' IT suites run against the same dev
// Postgres, and a fixed name would let one worktree's DROP/recreate pull
// the floor out from under another's mid-test run (#35). variant keeps
// the per-suite prefix (ite/itx/itr/…) so it-db-leftovers litter from a
// killed run stays attributable. Callers keep their own pool/migration
// setup — search_path and schemas differ per stack.
func mintITDB(t *testing.T, variant string) string {
	t.Helper()
	ctx := context.Background()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	dbName := "malxchange_it" + variant + "_" + hex.EncodeToString(suffix)

	adminURL := fmt.Sprintf("postgres://%s:%s@%s:5432/postgres",
		os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW, itPGHost)
	drop := func() error {
		admin, err := pgxpool.New(ctx, adminURL)
		if err != nil {
			return err
		}
		defer admin.Close()
		_, err = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
		return err
	}
	// Defensive pre-create drop: cleanups fire on normal exit only, so a
	// killed run leaks the database and CREATE would fail on the leftover.
	if err := drop(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drop() })

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatal(err)
	}
	admin.Close()

	return dbName
}

// itStack: throwaway schema + in-process broker + engine wired for manual
// batch driving (applyFetches is called directly, so "crash points" are
// expressible).
type itStack struct {
	pool     *pgxpool.Pool
	cfg      config.Kafka
	producer *kgo.Client
	broker   *kfake.Cluster
	engine   *Engine
	consumer *kgo.Client
	t        *testing.T
}

func newStack(t *testing.T) *itStack {
	t.Helper()
	ctx := context.Background()

	dbName := mintITDB(t, "e")

	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable&search_path=exchange",
		os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW, itPGHost, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, _ = pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS exchange`)
	if err := pgmig.Run(pgURL(t, dbName), migrations.FS, "exchange"); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	broker, err := kfake.NewCluster(kfake.NumBrokers(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	addrs := broker.ListenAddrs()
	if len(addrs) == 0 {
		t.Fatal("kfake gave no listen addresses")
	}
	addr := addrs[0]
	cfg := config.Kafka{Seeds: []string{addr}} // no SASL against kfake
	producer, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(producer.Close)
	if err := broker.CreateTopic(events.TopicOrderCommand, 1, nil); err != nil {
		t.Fatal(err)
	}

	s := &itStack{pool: pool, cfg: cfg, producer: producer, broker: broker, t: t}
	s.newEngine()
	return s
}

func (s *itStack) newEngine() {
	s.t.Helper()
	s.engine = NewEngine(s.pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), outbox.New(s.pool))
	if err := s.engine.Recover(context.Background()); err != nil {
		s.t.Fatalf("recover: %v", err)
	}
}

func (s *itStack) newConsumer(group string) {
	s.t.Helper()
	c, err := kafka.NewConsumer(s.cfg, group, []string{events.TopicOrderCommand},
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		s.t.Fatal(err)
	}
	s.consumer = c
	s.t.Cleanup(c.Close)
}

func pgURL(t *testing.T, db string) string {
	t.Helper()
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(os.Getenv("MAL_TEST_PG_SUPERUSER"), itPGPW),
		Host:     itPGHost + ":5432",
		Path:     "/" + db,
		RawQuery: "sslmode=disable&search_path=exchange",
	}
	return u.String()
}

func (s *itStack) produce(cmds ...events.OrderCommand) {
	s.t.Helper()
	ctx := context.Background()
	for i, c := range cmds {
		envID := fmt.Sprintf("%s-%d", c.OrderID, i)
		_, body, err := events.New(envID, c.Market, events.TopicOrderCommand, time.Now().UTC(), c)
		if err != nil {
			s.t.Fatal(err)
		}
		if err := s.producer.ProduceSync(ctx, &kgo.Record{
			Topic: events.TopicOrderCommand, Key: []byte(c.Market), Value: body,
		}).FirstErr(); err != nil {
			s.t.Fatal(err)
		}
	}
}

// drain applies polled batches until quiet (no records for a moment).
func (s *itStack) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	quiet := 0
	for quiet < 3 {
		fetches := s.consumer.PollRecords(ctx, 100)
		n := countRecords(fetches)
		if n == 0 {
			quiet++
			continue
		}
		quiet = 0
		if err := applyFetches(ctx, s.engine, s.consumer, fetches); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
}

// crashAfter: applies one poll batch but SKIPS the offset commit — the
// crash window between the Postgres commit and the offset commit.
func (s *itStack) crashAfter(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		fetches := s.consumer.PollRecords(ctx, 100)
		if countRecords(fetches) == 0 {
			return
		}
		if err := applyFetches(ctx, s.engine, s.consumer, fetches); err != nil {
			t.Fatalf("apply: %v", err)
		}
		// offsets NOT committed: simulate process death right after the tx
		s.newEngine() // fresh in-memory state, like a restarted process
	}
}

// countRecords counts without consuming the one-shot RecordsAll iterator.
func countRecords(fs kgo.Fetches) int {
	n := 0
	for _, f := range fs {
		for _, tp := range f.Topics {
			n += len(tp.Partitions[0].Records)
		}
	}
	return n
}

func placeCmd(id, member, side, kind, price, amount string) events.OrderCommand {
	return events.OrderCommand{Op: "place", OrderID: id, Member: member, Market: "BTC/USDT",
		Side: side, Kind: kind, Price: price, Amount: amount}
}

func TestFoldPersistsAndPublishes(t *testing.T) {
	s := newStack(t)
	s.newConsumer("fold-1")
	s.produce(
		placeCmd("m1", "alice", "sell", "limit", "50000.10", "0.0005"),
		placeCmd("t1", "bob", "buy", "limit", "50000.10", "0.0002"),
	)
	s.drain(t)

	var status string
	var filled, turnover string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status, filled_amount::text, turnover::text FROM orders WHERE id='t1'`).Scan(&status, &filled, &turnover); err != nil {
		t.Fatal(err)
	}
	// NUMERIC columns normalize text (10.00 → 10); compare as decimals.
	if status != "filled" || filled != "0.0002" || turnover != "10" {
		t.Errorf("t1 = %s %s %s, want filled 0.0002 / 10", status, filled, turnover)
	}

	var price, amount, buyFee string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT price::text, amount::text, buy_fee::text FROM fills WHERE market='BTC/USDT'`).Scan(&price, &amount, &buyFee); err != nil {
		t.Fatal(err)
	}
	// taker buy: 0.0002 BTC × 0.001 = 0.0000002 base fee
	if price != "50000.1" || amount != "0.0002" || buyFee != "0.0000002" {
		t.Errorf("fill = %s %s fee %s", price, amount, buyFee)
	}

	// outbox carries the fill, completion, and plate events
	for _, topic := range []string{events.TopicTradeFill, events.TopicOrderCompleted, events.TopicMarketPlate} {
		var n int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM outbox WHERE topic=$1 AND status IN ('pending','publishing','published')`, topic).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("no outbox rows for %s", topic)
		}
	}
	// m1 remains open with the partial fill
	var m1status string
	_ = s.pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id='m1'`).Scan(&m1status)
	if m1status != "open" {
		t.Errorf("m1 status = %s, want open (partial)", m1status)
	}
}

// TestSweptSidePublishesEmptyPlate: a fully-consumed Book side must
// publish as a present-but-empty side on market.plate ("bids":[]), the
// wire shape consumers need to clear that side's Depth. While the sides
// carried omitempty the shape was deleted from the event, so stale
// phantom levels survived every publish forever (audit issue #79).
func TestSweptSidePublishesEmptyPlate(t *testing.T) {
	s := newStack(t)
	s.newConsumer("sweep-1")
	// bob's crossing limit sell fully consumes alice's bid; the batch
	// ends with the bids side empty and the asks side never touched.
	s.produce(
		placeCmd("b1", "alice", "buy", "limit", "50000", "0.5"),
		placeCmd("t1", "bob", "sell", "limit", "49999", "0.5"),
	)
	s.drain(t)

	var filled string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status FROM orders WHERE id='b1'`).Scan(&filled); err != nil {
		t.Fatal(err)
	}
	if filled != "filled" {
		t.Fatalf("b1 status = %s, want filled (the sweep premise)", filled)
	}

	var payload string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE topic=$1 ORDER BY created_at DESC, id DESC LIMIT 1`,
		events.TopicMarketPlate).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"bids":[]`) {
		t.Errorf("last market.plate payload lacks the emptied bids side:\n%s", payload)
	}
	if !strings.Contains(payload, `"asks":null`) {
		t.Errorf("last market.plate payload should carry the untouched asks side as null:\n%s", payload)
	}
}

func TestCrashRedeliveryAppliesExactlyOnce(t *testing.T) {
	s := newStack(t)
	s.newConsumer("crash-1")
	s.produce(
		placeCmd("m1", "alice", "sell", "limit", "50000.10", "0.0005"),
		placeCmd("t1", "bob", "buy", "limit", "50000.10", "0.0002"),
	)
	s.crashAfter(t) // applies, never commits offsets; engine state reset

	// A "restarted" consumer redelivers everything from the group offset.
	s2 := &itStack{pool: s.pool, cfg: s.cfg, producer: s.producer, t: t}
	s2.newEngine()
	s.consumer.Close()
	s.newConsumerReuseEngine(s2.engine, "crash-1")
	s.drain(t)

	var fills int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM fills`).Scan(&fills); err != nil {
		t.Fatal(err)
	}
	if fills != 1 {
		t.Errorf("fills after redelivery = %d, want exactly 1", fills)
	}
	var orders int
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&orders)
	if orders != 2 {
		t.Errorf("orders after redelivery = %d, want 2", orders)
	}
}

// newConsumerReuseEngine attaches a consumer to a specific engine (the
// restarted one) without replacing it.
func (s *itStack) newConsumerReuseEngine(e *Engine, group string) {
	s.t.Helper()
	c, err := kafka.NewConsumer(s.cfg, group, []string{events.TopicOrderCommand},
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		s.t.Fatal(err)
	}
	s.consumer = c
	s.t.Cleanup(c.Close)
	s.engine = e
}

// statusFor picks one Market's snapshot. Status() publishes one snapshot
// per loaded Market — positional indexing silently grabs whichever
// Market the map hands over first (#26).
func statusFor(t *testing.T, e *Engine, market string) StatusSnapshot {
	t.Helper()
	for _, st := range e.Status() {
		if st.Market == market {
			return st
		}
	}
	t.Fatalf("no status snapshot for market %s", market)
	return StatusSnapshot{}
}

func TestRecoveryMatchesIdentically(t *testing.T) {
	s := newStack(t)
	s.newConsumer("recover-1")
	// min_turnover 5 forces amounts ≥ ~0.001 at these prices.
	s.produce(
		placeCmd("m1", "alice", "sell", "limit", "5001.10", "0.002"),
		placeCmd("m2", "carol", "sell", "limit", "5002.20", "0.002"),
		placeCmd("t1", "bob", "buy", "limit", "5001.10", "0.0011"), // partial on m1
	)
	s.drain(t)

	pre := statusFor(t, s.engine, "BTC/USDT")

	// Restart: fresh engine rebuilds from Postgres.
	s.newEngine()
	post := statusFor(t, s.engine, "BTC/USDT")
	if post.OpenOrders != pre.OpenOrders {
		t.Errorf("open orders after recovery = %d, want %d", post.OpenOrders, pre.OpenOrders)
	}
	if post.LastSeq < pre.LastSeq {
		t.Errorf("lastSeq regressed: %d < %d", post.LastSeq, pre.LastSeq)
	}

	// The recovered book continues matching correctly: cross m1's rest.
	s2 := &itStack{pool: s.pool, cfg: s.cfg, producer: s.producer, t: t}
	s2.engine = s.engine
	s.newConsumerReuseEngine(s.engine, "recover-2")
	s.produce(placeCmd("t2", "dave", "buy", "limit", "5001.10", "0.0011"))
	s.drain(t)

	var status string
	var filled string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT status, filled_amount::text FROM orders WHERE id='m1'`).Scan(&status, &filled); err != nil {
		t.Fatal(err)
	}
	if status != "filled" || money.MustParse(filled).String() != "0.002" {
		t.Errorf("m1 after recovery cross = %s %s, want filled 0.002", status, filled)
	}
}

func TestUnknownCancelRejected(t *testing.T) {
	s := newStack(t)
	s.newConsumer("cancel-1")
	s.produce(events.OrderCommand{Op: "cancel", OrderID: "ghost", Member: "bob", Market: "BTC/USDT"})
	s.drain(t)

	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE topic=$1`, events.TopicCancelRejected).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("unknown cancel produced no cancel.rejected event")
	}
}

func TestSubmissionRulesReject(t *testing.T) {
	s := newStack(t)
	// seeded BTC/USDT has min_turnover 5; 40000.10×0.0001 = 4.00001 < 5
	s.newConsumer("rules-1")
	s.produce(placeCmd("tiny", "bob", "buy", "limit", "40000.10", "0.0001"))
	s.drain(t)

	var status string
	err := s.pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id='tiny'`).Scan(&status)
	if err == nil {
		t.Fatalf("sub-minimum order persisted with status %s", status)
	}
	var n int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE topic=$1`, events.TopicCancelRejected).Scan(&n)
	if n == 0 {
		t.Error("no rejection event for sub-minimum order")
	}
}

var _ = uuid.NewString
