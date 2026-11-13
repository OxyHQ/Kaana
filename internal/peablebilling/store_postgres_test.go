package peablebilling

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func unique() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type fixture struct {
	ctx                        context.Context
	admin, runtime, migrator   *Store
	ledgerAdmin, ledgerRuntime *billing.Store
	runtimeDB                  *pgxpool.Pool
}

func database(t *testing.T) *fixture {
	t.Helper()
	raw := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if raw == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	root, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := root.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-test-role-provision'))`); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"kaana_billing_migrator", "kaana_billing_admin", "kaana_billing_runtime", "kaana_peable_billing_admin", "kaana_peable_billing_runtime"} {
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
	name := "kaana_peable_test_" + unique()
	if _, err = root.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := root.Exec(context.Background(), `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH(FORCE)`)
		root.Close()
		if e != nil {
			t.Error(e)
		}
	})
	own := cfg.Copy()
	own.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err = db.Exec(ctx, `GRANT CREATE ON DATABASE `+pgx.Identifier{name}.Sanitize()+` TO kaana_billing_migrator; GRANT USAGE,CREATE ON SCHEMA public TO kaana_billing_migrator`); err != nil {
		t.Fatal(err)
	}
	pool := func(role string) *pgxpool.Pool {
		c := own.Copy()
		c.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, e := conn.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize())
			return e
		}
		p, e := pgxpool.NewWithConfig(ctx, c)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(p.Close)
		return p
	}
	migrator := pool("kaana_billing_migrator")
	if err = billing.NewStore(migrator).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := NewStore(migrator)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	runtime := pool("kaana_peable_billing_runtime")
	return &fixture{ctx: ctx, admin: NewStore(pool("kaana_peable_billing_admin")), runtime: NewStore(runtime), migrator: s, ledgerAdmin: billing.NewStore(pool("kaana_billing_admin")), ledgerRuntime: billing.NewStore(pool("kaana_billing_runtime")), runtimeDB: runtime}
}
func (f *fixture) account(t *testing.T) billing.Account {
	t.Helper()
	a, e := f.ledgerAdmin.CreateAccount(f.ctx, "opaque-owner-"+unique(), "USD")
	if e != nil {
		t.Fatal(e)
	}
	b, e := f.ledgerAdmin.ConfigureBudget(f.ctx, billing.BudgetRequest{AccountID: a.ID, Key: "reviewed", Amount: "100", StartsAt: time.Now().Add(-time.Hour), EndsAt: time.Now().Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.ledgerAdmin.ConfigureAccount(f.ctx, a.ID, b, "100", true); e != nil {
		t.Fatal(e)
	}
	if e = f.admin.BindCustomer(f.ctx, a.ID, "merch_kaana", "cus_"+a.ID); e != nil {
		t.Fatal(e)
	}
	return a
}
func (f *fixture) receipt(t *testing.T, a billing.Account, amount string) billing.Receipt {
	t.Helper()
	h, e := f.ledgerRuntime.Reserve(f.ctx, billing.ReserveRequest{AccountID: a.ID, Key: unique(), AdmissionID: unique(), PriceSnapshotID: "price_v1", PriceDigest: strings.Repeat("a", 64), Maximum: amount, TTL: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	s, e := f.ledgerRuntime.Settle(f.ctx, billing.SettleRequest{AccountID: a.ID, HoldID: h.ID, Key: unique(), EvidenceID: unique(), EvidenceDigest: strings.Repeat("b", 64), Outcome: "completed", HasUsage: true, RatedAmount: amount})
	if e != nil || s.Receipt == nil {
		t.Fatalf("settlement failed: %v", e)
	}
	return *s.Receipt
}
func TestPostgresDraftUsesOnlyReceivableAndRoundsSubtotal(t *testing.T) {
	f := database(t)
	a := f.account(t)
	for kind, amount := range map[string]string{"promotional": "0.2", "purchased": "0.3"} {
		_, e := f.ledgerAdmin.Fund(f.ctx, billing.FundingRequest{AccountID: a.ID, Key: unique(), Kind: kind, Amount: amount, EvidenceID: unique()})
		if e != nil {
			t.Fatal(e)
		}
	}
	r := f.receipt(t, a, "1.235")
	d, e := f.admin.Prepare(f.ctx, a.ID, "batch", []string{r.ID}, 30)
	if e != nil {
		t.Fatal(e)
	}
	if d.Subtotal != "0.735000000000" || d.AmountMinor != "74" || d.RoundingAdjustment != "0.005000000000" {
		t.Fatalf("wrong invoice split/rounding: %#v", d)
	}
	again, e := f.admin.Prepare(f.ctx, a.ID, "batch", []string{r.ID}, 30)
	if e != nil || again.ID != d.ID {
		t.Fatalf("stable replay failed: %v", e)
	}
	if _, e = f.admin.Prepare(f.ctx, a.ID, "other", []string{r.ID}, 30); e == nil {
		t.Fatal("receipt could be invoiced twice")
	}
	if _, e = f.admin.Prepare(f.ctx, a.ID, "batch", []string{r.ID}, 31); e == nil {
		t.Fatal("terms replay changed")
	}

	downAccount := f.account(t)
	downReceipt := f.receipt(t, downAccount, "1.234")
	down, e := f.admin.Prepare(f.ctx, downAccount.ID, "down", []string{downReceipt.ID}, 30)
	if e != nil || down.AmountMinor != "123" || down.RoundingAdjustment != "-0.004000000000" {
		t.Fatalf("downward rounding lost: %v %#v", e, down)
	}
	tinyAccount := f.account(t)
	tiny := f.receipt(t, tinyAccount, "0.000001")
	if _, e = f.admin.Prepare(f.ctx, tinyAccount.ID, "tiny", []string{tiny.ID}, 30); e == nil {
		t.Fatal("zero minor units became payable")
	}
	rest := f.receipt(t, tinyAccount, "0.009999")
	combined, e := f.admin.Prepare(f.ctx, tinyAccount.ID, "combined", []string{tiny.ID, rest.ID}, 30)
	if e != nil || combined.AmountMinor != "1" {
		t.Fatal("refused tiny batch consumed/dropped receipt")
	}
	b := f.account(t)
	r1 := f.receipt(t, b, "0.005")
	r2 := f.receipt(t, b, "0.005")
	sum, e := f.admin.Prepare(f.ctx, b.ID, "sum", []string{r1.ID, r2.ID}, 15)
	if e != nil || sum.AmountMinor != "1" || sum.RoundingAdjustment != "0.000000000000" {
		t.Fatalf("rounded each receipt instead of total: %v %#v", e, sum)
	}
}
func TestPostgresDraftAuthorityAndCompensation(t *testing.T) {
	f := database(t)
	a := f.account(t)
	b := f.account(t)
	r := f.receipt(t, a, "1")
	if _, e := f.admin.Prepare(f.ctx, b.ID, "foreign", []string{r.ID}, 30); e == nil {
		t.Fatal("foreign receipt accepted")
	}
	if _, e := f.runtime.Prepare(f.ctx, a.ID, "forbidden", []string{r.ID}, 30); e == nil {
		t.Fatal("delivery runtime prepared billing terms")
	}
	if e := f.runtime.BindCustomer(f.ctx, a.ID, "merchant_foreign", "customer_foreign"); e == nil {
		t.Fatal("runtime remapped customer")
	}
	if _, e := f.runtimeDB.Exec(f.ctx, `UPDATE kaana_peable_billing.drafts SET amount_minor=999`); e == nil {
		t.Fatal("runtime direct DML allowed")
	}
	if _, e := billing.NewStore(f.runtimeDB).Fund(f.ctx, billing.FundingRequest{AccountID: a.ID, Key: unique(), Kind: "purchased", Amount: "1", EvidenceID: unique()}); e == nil {
		t.Fatal("delivery runtime minted funds")
	}
	d, e := f.admin.Prepare(f.ctx, a.ID, "batch", []string{r.ID}, 30)
	if e != nil {
		t.Fatal(e)
	}
	fresh, e := f.runtime.Claim(f.ctx, d.ID, "invoice")
	if e != nil || !fresh {
		t.Fatal("authorized claim failed")
	}
	fresh, e = f.runtime.Claim(f.ctx, d.ID, "invoice")
	if e != nil || fresh {
		t.Fatal("ambiguous claim could be repeated")
	}
	if e = f.runtime.Record(f.ctx, d.ID, "invoice", "in_own"); e != nil {
		t.Fatal(e)
	}
	if e = f.runtime.Record(f.ctx, d.ID, "invoice", "in_other"); e == nil {
		t.Fatal("external identity changed")
	}
	_, e = f.ledgerAdmin.Refund(f.ctx, billing.RefundRequest{AccountID: a.ID, ReceiptID: r.ID, Key: unique(), Amount: "0.1", EvidenceID: unique()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.runtime.Read(f.ctx, d.ID); e == nil {
		t.Fatal("compensation after snapshot was ignored")
	}
}
func TestPostgresDraftClaimConcurrentAndMigrationIntegrity(t *testing.T) {
	f := database(t)
	a := f.account(t)
	r := f.receipt(t, a, "1")
	d, e := f.admin.Prepare(f.ctx, a.ID, "batch", []string{r.ID}, 30)
	if e != nil {
		t.Fatal(e)
	}
	results := make(chan bool, 12)
	errs := make(chan error, 12)
	for range 12 {
		go func() { fresh, err := f.runtime.Claim(f.ctx, d.ID, "invoice"); results <- fresh; errs <- err }()
	}
	count := 0
	for range 12 {
		if <-results {
			count++
		}
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	if count != 1 {
		t.Fatalf("%d fresh attempts", count)
	}
	if e = f.runtime.Migrate(f.ctx); e == nil {
		t.Fatal("delivery runtime migrated schema")
	}
	_, e = f.migrator.pool.Exec(f.ctx, `UPDATE public.kaana_peable_draft_migrations SET checksum='bad'`)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.migrator.Migrate(f.ctx); e == nil {
		t.Fatal("migration drift accepted")
	}
}
