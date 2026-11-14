package billingaccounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func unique() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type fixture struct {
	ctx          context.Context
	db, migrator *pgxpool.Pool
	operator     *Store
	config       *pgxpool.Config
	login        string
	scope        Config
}

func database(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test DSN")
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
	for _, r := range []string{"kaana_billing_migrator", "kaana_billing_admin", "kaana_billing_runtime", "kaana_billing_account_admin"} {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, r).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err = tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{r}.Sanitize()+" NOLOGIN"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	name := "kbaccounts_" + unique()
	login := "kbaop_" + unique()
	password := unique()
	if _, err = root.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := root.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH(FORCE)")
		if e != nil {
			t.Error(e)
		}
		_, e = root.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{login}.Sanitize())
		if e != nil {
			t.Error(e)
		}
		root.Close()
	})
	own := cfg.Copy()
	own.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err = db.Exec(ctx, "GRANT CREATE ON DATABASE "+pgx.Identifier{name}.Sanitize()+" TO kaana_billing_migrator; GRANT USAGE,CREATE ON SCHEMA public TO kaana_billing_migrator; CREATE ROLE "+pgx.Identifier{login}.Sanitize()+" LOGIN NOINHERIT PASSWORD '"+password+"'; GRANT kaana_billing_account_admin TO "+pgx.Identifier{login}.Sanitize()+" WITH INHERIT FALSE, SET TRUE, ADMIN FALSE"); err != nil {
		t.Fatal(err)
	}
	mig := own.Copy()
	mig.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, e := c.Exec(ctx, "SET ROLE kaana_billing_migrator")
		return e
	}
	mp, err := pgxpool.NewWithConfig(ctx, mig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mp.Close)
	if err = billing.NewStore(mp).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = NewStore(mp).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	opcfg := own.Copy()
	opcfg.ConnConfig.User = login
	opcfg.ConnConfig.Password = password
	op, closePool, err := openConfig(ctx, opcfg, "provision")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePool)
	return &fixture{ctx: ctx, db: db, migrator: mp, operator: op, config: opcfg, login: login, scope: Config{Enabled: true, OwnerAccountID: "opaque-owner", Currency: "USD", Digest: strings.Repeat("a", 64)}}
}
func apply(t *testing.T, f *fixture, o Operation) map[string]any {
	t.Helper()
	raw, err := f.operator.Apply(f.ctx, f.scope, o)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func provision(t *testing.T, f *fixture) map[string]any {
	return apply(t, f, Operation{Action: "provision", Key: "create", EvidenceID: "review-create"})["snapshot"].(map[string]any)
}
func configure(snapshot map[string]any, key string) Operation {
	enabled := true
	return Operation{Action: "configure", AccountID: snapshot["accountId"].(string), Key: key, EvidenceID: "review-" + key, Revision: snapshot["revision"].(string), BudgetKey: "budget-" + key, StartsAt: time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z"), EndsAt: time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.000Z"), Amount: "10.000000000001", CreditLimit: "5.000000000001", Enabled: &enabled}
}
func inspect(t *testing.T, f *fixture, id string) map[string]any {
	return apply(t, f, Operation{Action: "inspect", AccountID: id})
}
func TestPostgresAccountOperatorAtomicReplayAndCAS(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	id := initial["accountId"].(string)
	if initial["enabled"] != false || initial["budget"] != nil || initial["purchased"] != "0.000000000000" {
		t.Fatal(initial)
	}
	first := configure(initial, "first")
	receipt := apply(t, f, first)
	current := inspect(t, f, id)
	second := configure(current, "second")
	disabled := false
	second.Enabled = &disabled
	second.CreditLimit = "0"
	apply(t, f, second)
	replay := apply(t, f, first)
	a, _ := json.Marshal(receipt)
	b, _ := json.Marshal(replay)
	if string(a) != string(b) {
		t.Fatal("lost-response replay changed receipt")
	}
	if inspect(t, f, id)["enabled"] != false {
		t.Fatal("historical replay reenabled account")
	}
	if _, err := f.operator.Apply(f.ctx, f.scope, configure(initial, "stale")); err == nil {
		t.Fatal("stale review accepted")
	}
	changed := first
	changed.Amount = "11"
	if _, err := f.operator.Apply(f.ctx, f.scope, changed); err == nil {
		t.Fatal("key reused with new terms")
	}
	if _, err := f.operator.Apply(f.ctx, f.scope, Operation{Action: "provision", Key: "new-create", EvidenceID: "new"}); err == nil {
		t.Fatal("configured account claimed empty")
	}
	if got := apply(t, f, Operation{Action: "provision", Key: "create", EvidenceID: "review-create"})["snapshot"].(map[string]any); got["enabled"] != false || got["budget"] != nil {
		t.Fatal("provision receipt was rewritten")
	}
	foreign := f.scope
	foreign.OwnerAccountID = "other"
	if _, err := f.operator.Apply(f.ctx, foreign, Operation{Action: "inspect", AccountID: id}); err == nil {
		t.Fatal("foreign owner read")
	}
	foreign = f.scope
	foreign.Currency = "EUR"
	if _, err := f.operator.Apply(f.ctx, foreign, first); err == nil {
		t.Fatal("foreign currency configured")
	}
	var budgets, operations int
	if err := f.db.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM kaana_billing.budgets),(SELECT count(*) FROM kaana_billing_accounts.operations)`).Scan(&budgets, &operations); err != nil || budgets != 2 || operations != 3 {
		t.Fatalf("failed operations left state %d/%d %v", budgets, operations, err)
	}
}
func TestPostgresAccountOperatorConcurrentReviews(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	one, two := configure(initial, "one"), configure(initial, "two")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, o := range []Operation{one, two} {
		wg.Add(1)
		go func(o Operation) { defer wg.Done(); _, e := f.operator.Apply(f.ctx, f.scope, o); results <- e }(o)
	}
	wg.Wait()
	close(results)
	passed, failed := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else {
			failed++
		}
	}
	if passed != 1 || failed != 1 {
		t.Fatalf("competing CAS %d/%d", passed, failed)
	}
}
func TestPostgresAccountOperatorRollbackAndExistingHolds(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	id := initial["accountId"].(string)
	o := configure(initial, "invalid")
	o.CreditLimit = "not-money"
	if _, err := f.operator.Apply(f.ctx, f.scope, o); err == nil {
		t.Fatal("invalid decimal accepted")
	}
	if inspect(t, f, id)["revision"] != initial["revision"] {
		t.Fatal("failed config changed account")
	}
	o = configure(initial, "valid")
	apply(t, f, o)
	before := inspect(t, f, id)
	runtime := billing.NewStore(f.db)
	hold, err := runtime.Reserve(f.ctx, billing.ReserveRequest{AccountID: id, Key: "reserve", AdmissionID: "admit", PriceSnapshotID: "price", PriceDigest: strings.Repeat("b", 64), Maximum: "1.000000000001", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.operator.Apply(f.ctx, f.scope, configure(before, "race-hold")); err == nil {
		t.Fatal("hold change bypassed review")
	}
	after := inspect(t, f, id)
	next := configure(after, "after-hold")
	apply(t, f, next)
	settled, err := runtime.Settle(f.ctx, billing.SettleRequest{AccountID: id, HoldID: hold.ID, Key: "settle", EvidenceID: "usage", EvidenceDigest: strings.Repeat("c", 64), Outcome: "completed", HasUsage: true, RatedAmount: "0.500000000001"})
	if err != nil || settled.Receipt == nil || settled.Receipt.Charged != "0.500000000001" {
		t.Fatalf("old hold lost %v %v", settled, err)
	}
	var held, spent string
	if err = f.db.QueryRow(f.ctx, `SELECT held::text,spent::text FROM kaana_billing.budgets WHERE id=$1`, hold.BudgetID).Scan(&held, &spent); err != nil || held != "0.000000000000" || spent != "0.500000000001" {
		t.Fatal(held, spent, err)
	}
}
func TestPostgresAccountOperatorPermissionsAndMigration(t *testing.T) {
	f := database(t)
	if err := NewStore(f.migrator).Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.operator.Migrate(f.ctx); err == nil {
		t.Fatal("account operator migrated")
	}
	_, err := f.operator.pool.Exec(f.ctx, `SELECT kaana_billing.fund('x','x','purchased','1','e')`)
	if err == nil {
		t.Fatal("operator funded")
	}
	_, err = f.operator.pool.Exec(f.ctx, `SELECT kaana_billing.refund('x','x','x','1','e')`)
	if err == nil {
		t.Fatal("operator compensated")
	}
	for _, sql := range []string{`DELETE FROM kaana_billing.accounts`, `TRUNCATE kaana_billing.accounts`, `INSERT INTO kaana_billing_accounts.operations VALUES('x','x','x','x','x','{}',now())`, `SET ROLE kaana_billing_admin`, `CREATE TABLE kaana_billing_accounts.forbidden(i int)`} {
		if _, err = f.operator.pool.Exec(f.ctx, sql); err == nil {
			t.Fatal("forbidden authority accepted", sql)
		}
	}
	// Real grants to the underlying login exercise RESET ROLE bypasses, not just
	// the current selected group. Each rejection has a restored positive control.
	login := pgx.Identifier{f.login}.Sanitize()
	cases := []struct{ name, grant, revoke string }{
		{"admin option", "GRANT kaana_billing_account_admin TO " + login + " WITH ADMIN TRUE", "REVOKE ADMIN OPTION FOR kaana_billing_account_admin FROM " + login},
		{"extra group", "GRANT kaana_billing_admin TO " + login, "REVOKE kaana_billing_admin FROM " + login},
		{"migration metadata DML", "GRANT UPDATE ON public.kaana_billing_account_migrations TO " + login, "REVOKE UPDATE ON public.kaana_billing_account_migrations FROM " + login},
		{"billing metadata DML", "GRANT DELETE ON public.kaana_billing_migrations TO kaana_billing_account_admin", "REVOKE DELETE ON public.kaana_billing_migrations FROM kaana_billing_account_admin"},
		{"destructive DML", "GRANT DELETE ON kaana_billing.accounts TO " + login, "REVOKE DELETE ON kaana_billing.accounts FROM " + login},
		{"session funding", "GRANT USAGE ON SCHEMA kaana_billing TO " + login + "; GRANT EXECUTE ON FUNCTION kaana_billing.fund(text,text,text,text,text) TO " + login, "REVOKE EXECUTE ON FUNCTION kaana_billing.fund(text,text,text,text,text) FROM " + login + "; REVOKE USAGE ON SCHEMA kaana_billing FROM " + login},
		{"group funding", "GRANT USAGE ON SCHEMA kaana_billing TO kaana_billing_account_admin; GRANT EXECUTE ON FUNCTION kaana_billing.fund(text,text,text,text,text) TO kaana_billing_account_admin", "REVOKE EXECUTE ON FUNCTION kaana_billing.fund(text,text,text,text,text) FROM kaana_billing_account_admin; REVOKE USAGE ON SCHEMA kaana_billing FROM kaana_billing_account_admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := f.db.Exec(f.ctx, tc.grant); e != nil {
				t.Fatal(e)
			}
			_, closePool, e := openConfig(f.ctx, f.config, "inspect")
			if e == nil {
				closePool()
				t.Fatal("excess authority accepted")
			}
			if _, e = f.db.Exec(f.ctx, tc.revoke); e != nil {
				t.Fatal(e)
			}
			_, closePool, e = openConfig(f.ctx, f.config, "inspect")
			if e != nil {
				t.Fatal("restored authority refused", e)
			}
			closePool()
		})
	}
	if _, err = f.db.Exec(f.ctx, `UPDATE public.kaana_billing_migrations SET checksum='changed'`); err != nil {
		t.Fatal(err)
	}
	if err = NewStore(f.migrator).Migrate(f.ctx); err == nil {
		t.Fatal("changed prerequisite accepted")
	}
}
func TestPostgresAccountOperatorReceiptFailureRollsBack(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	id := initial["accountId"].(string)
	if _, err := f.db.Exec(f.ctx, `CREATE FUNCTION public.fail_account_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic persistence failure'; END $$; CREATE TRIGGER fail_receipt BEFORE INSERT ON kaana_billing_accounts.operations FOR EACH ROW EXECUTE FUNCTION public.fail_account_receipt()`); err != nil {
		t.Fatal(err)
	}
	op := configure(initial, "atomic")
	if _, err := f.operator.Apply(f.ctx, f.scope, op); err == nil {
		t.Fatal("receipt failure committed")
	}
	if inspect(t, f, id)["revision"] != initial["revision"] {
		t.Fatal("account mutated without receipt")
	}
	var count int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM kaana_billing.budgets`).Scan(&count); err != nil || count != 0 {
		t.Fatal("budget committed without receipt", err)
	}
	if _, err := f.db.Exec(f.ctx, `DROP TRIGGER fail_receipt ON kaana_billing_accounts.operations`); err != nil {
		t.Fatal(err)
	}
	apply(t, f, op)
}
func TestPostgresAccountOperatorConcurrentExactReplay(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	op := configure(initial, "once")
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := f.operator.Apply(f.ctx, f.scope, op)
			if err != nil {
				results <- err.Error()
			} else {
				results <- string(raw)
			}
		}()
	}
	wg.Wait()
	close(results)
	var receipts []string
	for result := range results {
		receipts = append(receipts, result)
	}
	if len(receipts) != 2 || receipts[0] != receipts[1] || !strings.Contains(receipts[0], `"snapshot"`) {
		t.Fatal("exact concurrent replay failed", receipts)
	}
	var count int
	if err := f.db.QueryRow(f.ctx, `SELECT count(*) FROM kaana_billing_accounts.operations WHERE operation_key='once'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestPostgresAccountOperatorFundedAccountAndMigrationLogin(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	id := initial["accountId"].(string)
	if _, err := billing.NewStore(f.db).Fund(f.ctx, billing.FundingRequest{AccountID: id, Key: "synthetic-fund", Kind: "purchased", Amount: "1", EvidenceID: "synthetic-payment"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.operator.Apply(f.ctx, f.scope, Operation{Action: "provision", Key: "new", EvidenceID: "review"}); err == nil {
		t.Fatal("funded account claimed empty")
	}
	// The source operator never exposes Fund; this fixture invokes the existing
	// admin library independently to test refusal of already funded accounts.
	login := pgx.Identifier{f.login}.Sanitize()
	if _, err := f.db.Exec(f.ctx, "REVOKE kaana_billing_account_admin FROM "+login+"; GRANT kaana_billing_migrator TO "+login+" WITH INHERIT FALSE, SET TRUE, ADMIN FALSE"); err != nil {
		t.Fatal(err)
	}
	s, closePool, err := openConfig(f.ctx, f.config, "migrate")
	if err != nil {
		t.Fatal(err)
	}
	defer closePool()
	if err = s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
}
func waitBlocked(t *testing.T, f *fixture, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := f.db.QueryRow(f.ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected account lock contention was not observed")
}
func configureTx(ctx context.Context, tx pgx.Tx, c Config, o Operation) error {
	var raw []byte
	return tx.QueryRow(ctx, `SELECT kaana_billing_accounts.configure($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, c.OwnerAccountID, c.Currency, o.AccountID, o.Key, o.EvidenceID, c.Digest, o.Revision, o.BudgetKey, o.StartsAt, o.EndsAt, o.Amount, o.CreditLimit, *o.Enabled).Scan(&raw)
}
func TestPostgresAccountOperatorReserveLinearization(t *testing.T) {
	f := database(t)
	initial := provision(t, f)
	id := initial["accountId"].(string)
	apply(t, f, configure(initial, "initial"))
	review := inspect(t, f, id)
	change := configure(review, "concurrent")
	reserve := billing.ReserveRequest{AccountID: id, Key: "reserve-first", AdmissionID: "reserve-first", PriceSnapshotID: "price", PriceDigest: strings.Repeat("b", 64), Maximum: "1", TTL: time.Minute}
	holding, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holding.Rollback(f.ctx) }()
	if _, err = billing.ReserveInTransaction(f.ctx, holding, reserve); err != nil {
		t.Fatal(err)
	}
	configuring, err := f.operator.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configuring.Rollback(f.ctx) }()
	done := make(chan error, 1)
	go func() { done <- configureTx(f.ctx, configuring, f.scope, change) }()
	waitBlocked(t, f, configuring.Conn().PgConn().PID())
	if err = holding.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("configuration ignored newly committed reservation")
	}
	_ = configuring.Rollback(f.ctx)
	// Opposite order: a configuration holding the account lock disables it.
	// A waiting reserve must evaluate that committed state, not the old review.
	review = inspect(t, f, id)
	change = configure(review, "disable-first")
	disabled := false
	change.Enabled = &disabled
	configuring, err = f.operator.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configuring.Rollback(f.ctx) }()
	if err = configureTx(f.ctx, configuring, f.scope, change); err != nil {
		t.Fatal(err)
	}
	holding, err = f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holding.Rollback(f.ctx) }()
	reserve.Key = "reserve-second"
	reserve.AdmissionID = "reserve-second"
	go func() { _, e := billing.ReserveInTransaction(f.ctx, holding, reserve); done <- e }()
	waitBlocked(t, f, holding.Conn().PgConn().PID())
	if err = configuring.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, billing.ErrDisabled) {
		t.Fatal("waiting reservation escaped disabled account", err)
	}
}
