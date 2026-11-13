package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	runtime, admin                   *Store
	db, migrator, runtimeDB, adminDB *pgxpool.Pool
	ctx                              context.Context
}

func unique(prefix string) string {
	var b [12]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func database(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid PostgreSQL test configuration")
	}
	root, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	// Dedicated cluster roles, no login/password or privileges on other schemas.
	tx, err := root.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-test-role-provision'))`); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"kaana_billing_migrator", "kaana_billing_admin", "kaana_billing_runtime"} {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err = tx.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` NOLOGIN`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	name := unique("kaana_billing_test_")
	if _, err = root.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := root.Exec(context.Background(), `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
		root.Close()
		if err != nil {
			t.Error(err)
		}
	})
	own := config.Copy()
	own.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err = db.Exec(ctx, `GRANT CREATE ON DATABASE `+pgx.Identifier{name}.Sanitize()+` TO kaana_billing_migrator; GRANT USAGE,CREATE ON SCHEMA public TO kaana_billing_migrator`); err != nil {
		t.Fatal(err)
	}
	rolePool := func(role string) *pgxpool.Pool {
		cfg := own.Copy()
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize())
			return err
		}
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	migrator := rolePool("kaana_billing_migrator")
	if err = NewStore(migrator).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	runtime, admin := rolePool("kaana_billing_runtime"), rolePool("kaana_billing_admin")
	return &fixture{runtime: NewStore(runtime), admin: NewStore(admin), db: db, migrator: migrator, runtimeDB: runtime, adminDB: admin, ctx: ctx}
}
func (f *fixture) account(t *testing.T, cap, credit string) Account {
	t.Helper()
	a, err := f.admin.CreateAccount(f.ctx, unique("opaque-owner-"), "USD")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.admin.ConfigureBudget(f.ctx, BudgetRequest{AccountID: a.ID, Key: "budget", Amount: cap, StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.admin.ConfigureAccount(f.ctx, a.ID, b, credit, true); err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *fixture) fund(t *testing.T, a Account, kind, amount string) {
	t.Helper()
	_, err := f.admin.Fund(f.ctx, FundingRequest{AccountID: a.ID, Key: unique("fund-"), Kind: kind, Amount: amount, EvidenceID: unique("verified-evidence-")})
	if err != nil {
		t.Fatal(err)
	}
}
func reserve(a Account, key, amount string) ReserveRequest {
	return ReserveRequest{AccountID: a.ID, Key: key, AdmissionID: unique("admission-"), PriceSnapshotID: "reviewed-price-v1", PriceDigest: strings.Repeat("a", 64), Maximum: amount, TTL: time.Minute}
}
func settlement(a Account, h Hold, amount string) SettleRequest {
	return SettleRequest{AccountID: a.ID, HoldID: h.ID, Key: unique("settle-"), EvidenceID: unique("usage-receipt-"), EvidenceDigest: strings.Repeat("b", 64), Outcome: "completed", HasUsage: true, RatedAmount: amount}
}
func balance(t *testing.T, f *fixture, a Account, p, q, i, h string) {
	t.Helper()
	b, err := f.runtime.Balance(f.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := Balance{p, q, i, h}
	if b != want {
		t.Fatalf("balance %#v; want %#v", b, want)
	}
}
func expectError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error %v; want %v", err, want)
	}
}

func TestPostgresBillingProvisionAndPermissions(t *testing.T) {
	f := database(t)
	owner := "opaque-owner-not-a-kaana-key"
	a, err := f.admin.CreateAccount(f.ctx, owner, "USD")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.admin.CreateAccount(f.ctx, owner, "USD")
	if err != nil || a != b {
		t.Fatalf("account retry %#v %v", b, err)
	}
	if a.Enabled || !strings.HasPrefix(a.ID, "kba_") || !strings.HasPrefix(a.CurrencyID, "kcur_") || a.ID == owner {
		t.Fatalf("invalid identity %#v", a)
	}
	_, err = f.runtime.Reserve(f.ctx, reserve(a, "disabled", "0"))
	expectError(t, err, ErrDisabled)
	if _, err = f.runtime.CreateAccount(f.ctx, "foreign", "USD"); err == nil {
		t.Fatal("runtime provisioned account")
	}
	if _, err = f.runtime.Fund(f.ctx, FundingRequest{AccountID: a.ID, Key: "mint", Kind: "purchased", Amount: "10", EvidenceID: "invented"}); err == nil {
		t.Fatal("runtime minted funds")
	}
	for _, query := range []string{
		`UPDATE kaana_billing.accounts SET purchased=1000`,
		`INSERT INTO kaana_billing.journal(account_id,operation_id,kind,source,destination,amount) SELECT id,'invented','funding','external_settlement','purchased',1000 FROM kaana_billing.accounts`,
		`SELECT kaana_billing.post('x','x','funding','external_settlement','purchased',10)`,
		`DELETE FROM kaana_billing.holds`,
	} {
		if _, err = f.runtimeDB.Exec(f.ctx, query); err == nil {
			t.Fatalf("runtime could execute forbidden statement %s", query)
		}
	}
	if err = f.runtime.Migrate(f.ctx); err == nil {
		t.Fatal("runtime migrated")
	}
	if err = NewStore(f.migrator).Migrate(f.ctx); err != nil {
		t.Fatalf("migration replay %v", err)
	}
	if _, err = f.db.Exec(f.ctx, `UPDATE public.kaana_billing_migrations SET checksum='corrupt'`); err != nil {
		t.Fatal(err)
	}
	expectError(t, NewStore(f.migrator).Migrate(f.ctx), ErrConflict)
}

func TestPostgresBillingDrawSettleAndCompensate(t *testing.T) {
	f := database(t)
	a := f.account(t, "30", "10")
	f.fund(t, a, "promotional", "3")
	f.fund(t, a, "purchased", "4")
	req := reserve(a, "draw", "9")
	h, err := f.runtime.Reserve(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if h.Replay {
		t.Fatal("fresh hold marked replay")
	}
	balance(t, f, a, "0.000000000000", "0.000000000000", "2.000000000000", "9.000000000000")
	replay, err := f.runtime.Reserve(f.ctx, req)
	if err != nil || !replay.Replay || replay.ID != h.ID {
		t.Fatalf("hold replay %#v %v", replay, err)
	}
	sr := settlement(a, h, "8")
	done, err := f.runtime.Settle(f.ctx, sr)
	if err != nil || done.Receipt == nil {
		t.Fatalf("settle %#v %v", done, err)
	}
	balance(t, f, a, "0.000000000000", "0.000000000000", "1.000000000000", "0.000000000000")
	again, err := f.runtime.Settle(f.ctx, sr)
	if err != nil || again.Receipt.ID != done.Receipt.ID {
		t.Fatalf("settle retry %#v %v", again, err)
	}
	refund := RefundRequest{AccountID: a.ID, ReceiptID: done.Receipt.ID, Key: "compensate", Amount: "3", EvidenceID: "approved-usage-correction"}
	fid, err := f.admin.Refund(f.ctx, refund)
	if err != nil {
		t.Fatal(err)
	}
	balance(t, f, a, "0.000000000000", "2.000000000000", "0.000000000000", "0.000000000000")
	fid2, err := f.admin.Refund(f.ctx, refund)
	if err != nil || fid2 != fid {
		t.Fatalf("refund retry %v", err)
	}
	refund.Key = "remainder"
	refund.Amount = "5"
	refund.EvidenceID = "approved-rest"
	if _, err = f.admin.Refund(f.ctx, refund); err != nil {
		t.Fatal(err)
	}
	balance(t, f, a, "3.000000000000", "4.000000000000", "0.000000000000", "0.000000000000")
	refund.Key = "over"
	refund.Amount = "0.000000000001"
	refund.EvidenceID = "over"
	_, err = f.admin.Refund(f.ctx, refund)
	expectError(t, err, ErrExceedsCharge)
	var net string
	if err = f.db.QueryRow(f.ctx, `SELECT sum(amount)::text FROM kaana_billing.postings`).Scan(&net); err != nil || net != "0.000000000000" {
		t.Fatalf("unbalanced %q %v", net, err)
	}
	var nonzero int
	if err = f.db.QueryRow(f.ctx, `SELECT count(*) FROM (SELECT journal_id FROM kaana_billing.postings GROUP BY journal_id HAVING sum(amount)<>0) x`).Scan(&nonzero); err != nil || nonzero != 0 {
		t.Fatalf("unbalanced entries %d %v", nonzero, err)
	}
	for _, table := range []string{"funding", "receipts", "refunds", "journal", "price_snapshots"} {
		if _, err = f.db.Exec(f.ctx, `DELETE FROM kaana_billing.`+table); err == nil {
			t.Fatalf("immutable %s deleted", table)
		}
	}
	if _, err = f.db.Exec(f.ctx, `UPDATE kaana_billing.holds SET amount=0`); err == nil {
		t.Fatal("hold rewritten")
	}
	if _, err = f.db.Exec(f.ctx, `UPDATE kaana_billing.accounts SET oxy_owner='changed'`); err == nil {
		t.Fatal("owner rewritten")
	}
}

func TestPostgresBillingConcurrentReservationAndBudget(t *testing.T) {
	f := database(t)
	a := f.account(t, "7", "0")
	f.fund(t, a, "purchased", "100")
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Go(func() { _, err := f.runtime.Reserve(f.ctx, reserve(a, unique("reserve-"), "1")); results <- err })
	}
	wg.Wait()
	close(results)
	good, budget := 0, 0
	for err := range results {
		if err == nil {
			good++
		} else if errors.Is(err, ErrBudget) {
			budget++
		} else {
			t.Fatal(err)
		}
	}
	if good != 7 || budget != 9 {
		t.Fatalf("admitted=%d rejected=%d", good, budget)
	}
	balance(t, f, a, "0.000000000000", "93.000000000000", "0.000000000000", "7.000000000000")
	// A separate account proves the credit ceiling independently of the budget.
	b := f.account(t, "100", "2")
	results = make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, err := f.runtime.Reserve(f.ctx, reserve(b, unique("credit-"), "1")); results <- err })
	}
	wg.Wait()
	close(results)
	good = 0
	for err := range results {
		if err == nil {
			good++
		} else if !errors.Is(err, ErrFunds) {
			t.Fatal(err)
		}
	}
	if good != 2 {
		t.Fatalf("credit admitted %d", good)
	}
}

func TestPostgresBillingConcurrentIdempotencyAndConflicts(t *testing.T) {
	f := database(t)
	a := f.account(t, "100", "0")
	f.fund(t, a, "purchased", "10")
	req := reserve(a, "same", "4")
	var wg sync.WaitGroup
	results := make(chan Hold, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() { h, err := f.runtime.Reserve(f.ctx, req); results <- h; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	fresh := 0
	id := ""
	for h := range results {
		if !h.Replay {
			fresh++
		}
		if id != "" && id != h.ID {
			t.Fatal("duplicate hold")
		}
		id = h.ID
	}
	if fresh != 1 {
		t.Fatalf("fresh holds %d", fresh)
	}
	req.Maximum = "5"
	_, err := f.runtime.Reserve(f.ctx, req)
	expectError(t, err, ErrConflict)
	// Immutable price IDs cannot be silently assigned another tariff digest.
	different := reserve(a, "changed-price", "1")
	different.PriceDigest = strings.Repeat("c", 64)
	_, err = f.runtime.Reserve(f.ctx, different)
	expectError(t, err, ErrConflict)
	balance(t, f, a, "0.000000000000", "6.000000000000", "0.000000000000", "4.000000000000")
	sr := settlement(a, Hold{ID: id}, "3")
	done, err := f.runtime.Settle(f.ctx, sr)
	if err != nil {
		t.Fatal(err)
	}
	sr.EvidenceDigest = strings.Repeat("d", 64)
	_, err = f.runtime.Settle(f.ctx, sr)
	expectError(t, err, ErrConflict)
	sr.Key = "other-key"
	_, err = f.runtime.Settle(f.ctx, sr)
	expectError(t, err, ErrConflict)
	refund := RefundRequest{AccountID: a.ID, ReceiptID: done.Receipt.ID, Key: "one-refund", Amount: "3", EvidenceID: "verified-refund"}
	errs = make(chan error, 10)
	for range 10 {
		wg.Go(func() { _, err := f.admin.Refund(f.ctx, refund); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	balance(t, f, a, "0.000000000000", "10.000000000000", "0.000000000000", "0.000000000000")
	refund.Amount = "2"
	_, err = f.admin.Refund(f.ctx, refund)
	expectError(t, err, ErrConflict)
}

func TestPostgresBillingFailedCancelledAndEmptyNotCharged(t *testing.T) {
	f := database(t)
	a := f.account(t, "10", "0")
	f.fund(t, a, "purchased", "10")
	for _, outcome := range []string{"failed", "cancelled", "completed", "partial"} {
		h, err := f.runtime.Reserve(f.ctx, reserve(a, unique("nocharge-"), "10"))
		if err != nil {
			t.Fatal(err)
		}
		sr := settlement(a, h, "0")
		sr.Outcome = outcome
		sr.HasUsage = false
		if outcome == "failed" || outcome == "cancelled" {
			sr.HasUsage = true
			sr.RatedAmount = "4"
		}
		done, err := f.runtime.Settle(f.ctx, sr)
		if err != nil || done.Receipt == nil || done.Receipt.Charged != "0.000000000000" {
			t.Fatalf("charged %s %#v %v", outcome, done, err)
		}
		balance(t, f, a, "0.000000000000", "10.000000000000", "0.000000000000", "0.000000000000")
	}
}

func TestPostgresBillingExpiryAndLateSettlement(t *testing.T) {
	f := database(t)
	a := f.account(t, "2", "0")
	f.fund(t, a, "purchased", "2")
	req := reserve(a, "expiry", "1")
	req.TTL = time.Second
	h, err := f.runtime.Reserve(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.runtime.Expire(f.ctx, 10)
	if err != nil || n != 0 {
		t.Fatalf("premature expiry %d %v", n, err)
	}
	// A distinct live hold is the uninterrupted control: expiry cannot release it.
	live, err := f.runtime.Reserve(f.ctx, reserve(a, "live", "1"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(h.ExpiresAt) + 20*time.Millisecond)
	late, err := f.runtime.Settle(f.ctx, settlement(a, h, "1"))
	if err != nil || late.State != "expired" || late.Receipt != nil {
		t.Fatalf("late %#v %v", late, err)
	}
	n, err = f.runtime.Expire(f.ctx, 10)
	if err != nil || n != 0 {
		t.Fatalf("double expiry %d %v", n, err)
	}
	balance(t, f, a, "0.000000000000", "1.000000000000", "0.000000000000", "1.000000000000")
	if _, err = f.runtime.Settle(f.ctx, settlement(a, live, "1")); err != nil {
		t.Fatal(err)
	}
	// Expiry-only path is also bounded and releases a fresh past-deadline hold.
	req = reserve(a, "sweep", "1")
	req.TTL = time.Second
	h, err = f.runtime.Reserve(f.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(h.ExpiresAt) + 20*time.Millisecond)
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			n, err := f.runtime.Expire(f.ctx, 1)
			if err != nil {
				t.Error(err)
			}
			counts <- n
		})
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if total != 1 {
		t.Fatalf("expiry count %d", total)
	}
	balance(t, f, a, "0.000000000000", "1.000000000000", "0.000000000000", "0.000000000000")
}

func TestPostgresBillingRollbackAndOversizedSettlement(t *testing.T) {
	f := database(t)
	a := f.account(t, "10", "0")
	f.fund(t, a, "purchased", "10")
	h, err := f.runtime.Reserve(f.ctx, reserve(a, "held", "4"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.runtime.Settle(f.ctx, settlement(a, h, "4.000000000001"))
	expectError(t, err, ErrExceedsHold)
	balance(t, f, a, "0.000000000000", "6.000000000000", "0.000000000000", "4.000000000000")
	// Force a failure AFTER receipt insertion and part of the journal, proving the
	// whole operation rolls back instead of leaving a receipt without its release.
	_, err = f.db.Exec(f.ctx, `CREATE FUNCTION kaana_billing.inject_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='release' THEN RAISE EXCEPTION 'test injected release failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER test_failure BEFORE INSERT ON kaana_billing.journal FOR EACH ROW EXECUTE FUNCTION kaana_billing.inject_failure()`)
	if err != nil {
		t.Fatal(err)
	}
	sr := settlement(a, h, "2")
	if _, err = f.runtime.Settle(f.ctx, sr); err == nil {
		t.Fatal("injected failure not observed")
	}
	var count int
	if err = f.db.QueryRow(f.ctx, `SELECT count(*) FROM kaana_billing.receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan receipt %d %v", count, err)
	}
	balance(t, f, a, "0.000000000000", "6.000000000000", "0.000000000000", "4.000000000000")
	if _, err = f.db.Exec(f.ctx, `DROP TRIGGER test_failure ON kaana_billing.journal`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.runtime.Settle(f.ctx, sr); err != nil {
		t.Fatal(err)
	}
	balance(t, f, a, "0.000000000000", "8.000000000000", "0.000000000000", "0.000000000000")
}

func TestPostgresBillingExactScaleAndScopedEvidence(t *testing.T) {
	f := database(t)
	a := f.account(t, "1", "0")
	f.fund(t, a, "purchased", "0.000000000003")
	for _, bad := range []string{"1e-12", "0.0000000000001", "-1", "NaN", "01", "1.0000000000000"} {
		_, err := f.runtime.Reserve(f.ctx, reserve(a, unique("invalid-"), bad))
		expectError(t, err, ErrInvalid)
	}
	h, err := f.runtime.Reserve(f.ctx, reserve(a, "pico", "0.000000000002"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := f.account(t, "10", "10")
	_, err = f.runtime.Settle(f.ctx, settlement(foreign, h, "0.000000000001"))
	expectError(t, err, ErrNotFound)
	done, err := f.runtime.Settle(f.ctx, settlement(a, h, "0.000000000001"))
	if err != nil || done.Receipt.Charged != "0.000000000001" {
		t.Fatalf("precision %#v %v", done, err)
	}
	balance(t, f, a, "0.000000000000", "0.000000000002", "0.000000000000", "0.000000000000")
	_, err = f.admin.Refund(f.ctx, RefundRequest{AccountID: foreign.ID, ReceiptID: done.Receipt.ID, Key: "foreign", Amount: "0.000000000001", EvidenceID: "foreign"})
	expectError(t, err, ErrNotFound)
	fund := FundingRequest{AccountID: a.ID, Key: "fund-evidence", Kind: "purchased", Amount: "1", EvidenceID: "payment-global"}
	if _, err = f.admin.Fund(f.ctx, fund); err != nil {
		t.Fatal(err)
	}
	fund.AccountID = foreign.ID
	fund.Key = "other-account"
	_, err = f.admin.Fund(f.ctx, fund)
	expectError(t, err, ErrConflict)
	balance(t, f, foreign, "0.000000000000", "0.000000000000", "0.000000000000", "0.000000000000")
}

func TestPostgresBillingBudgetWindowCurrencyAndFundingRetries(t *testing.T) {
	f := database(t)
	owner := unique("same-opaque-owner-")
	usd, err := f.admin.CreateAccount(f.ctx, owner, "USD")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := f.admin.CreateAccount(f.ctx, owner, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if usd.ID == eur.ID || usd.CurrencyID == eur.CurrencyID {
		t.Fatal("currencies share a balance")
	}
	starts := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	request := BudgetRequest{AccountID: usd.ID, Key: "future", Amount: "5", StartsAt: starts, EndsAt: starts.Add(time.Hour)}
	bid, err := f.admin.ConfigureBudget(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.admin.ConfigureBudget(f.ctx, request)
	if err != nil || again != bid {
		t.Fatalf("budget retry %q %v", again, err)
	}
	request.Amount = "6"
	_, err = f.admin.ConfigureBudget(f.ctx, request)
	expectError(t, err, ErrConflict)
	if err = f.admin.ConfigureAccount(f.ctx, usd.ID, bid, "5", true); err != nil {
		t.Fatal(err)
	}
	_, err = f.runtime.Reserve(f.ctx, reserve(usd, "early", "1"))
	expectError(t, err, ErrBudget)
	if err = f.admin.ConfigureAccount(f.ctx, eur.ID, bid, "5", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign budget accepted %v", err)
	}
	funding := FundingRequest{AccountID: usd.ID, Key: "payment", Kind: "purchased", Amount: "0.123456789012", EvidenceID: "processor-verified-payment"}
	id, err := f.admin.Fund(f.ctx, funding)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := f.admin.Fund(f.ctx, funding)
	if err != nil || id2 != id {
		t.Fatalf("payment retry %q %v", id2, err)
	}
	funding.Amount = "0.123456789013"
	_, err = f.admin.Fund(f.ctx, funding)
	expectError(t, err, ErrConflict)
	funding.Amount = "0.123456789012"
	funding.Key = "other-key"
	_, err = f.admin.Fund(f.ctx, funding)
	expectError(t, err, ErrConflict)
	balance(t, f, usd, "0.000000000000", "0.123456789012", "0.000000000000", "0.000000000000")
	balance(t, f, eur, "0.000000000000", "0.000000000000", "0.000000000000", "0.000000000000")
}

func TestPostgresBillingCompetingSettlementsAndRefundCaps(t *testing.T) {
	f := database(t)
	a := f.account(t, "10", "0")
	f.fund(t, a, "purchased", "10")
	h, err := f.runtime.Reserve(f.ctx, reserve(a, "competing", "10"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan Settlement, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { r, err := f.runtime.Settle(f.ctx, settlement(a, h, "8")); results <- r; errs <- err })
	}
	wg.Wait()
	close(results)
	close(errs)
	success, conflicts := 0, 0
	rid := ""
	for r := range results {
		if r.Receipt != nil {
			rid = r.Receipt.ID
		}
	}
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 7 {
		t.Fatalf("settlements success=%d conflicts=%d", success, conflicts)
	}
	errs = make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := f.admin.Refund(f.ctx, RefundRequest{AccountID: a.ID, ReceiptID: rid, Key: unique("refund-key-"), Amount: "2", EvidenceID: unique("approved-refund-")})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success = 0
	rejected := 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrExceedsCharge) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if success != 4 || rejected != 4 {
		t.Fatalf("refunds success=%d rejected=%d", success, rejected)
	}
	balance(t, f, a, "0.000000000000", "10.000000000000", "0.000000000000", "0.000000000000")
}

func TestPostgresBillingCancelledWaitDoesNotReserve(t *testing.T) {
	f := database(t)
	a := f.account(t, "10", "0")
	f.fund(t, a, "purchased", "10")
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err = tx.Exec(f.ctx, `SELECT id FROM kaana_billing.accounts WHERE id=$1 FOR UPDATE`, a.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 50*time.Millisecond)
	defer cancel()
	request := reserve(a, "cancelled-before-lock", "2")
	if _, err = f.runtime.Reserve(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled lock wait %v", err)
	}
	if err = tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	balance(t, f, a, "0.000000000000", "10.000000000000", "0.000000000000", "0.000000000000")
	// Same operation succeeds uninterrupted; cancellation did not consume its key.
	h, err := f.runtime.Reserve(f.ctx, request)
	if err != nil || h.Replay {
		t.Fatalf("control reservation %#v %v", h, err)
	}
	balance(t, f, a, "0.000000000000", "8.000000000000", "0.000000000000", "2.000000000000")
}
