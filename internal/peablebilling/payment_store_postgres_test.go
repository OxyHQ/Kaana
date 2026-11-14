package peablebilling

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func paymentDatabase(t *testing.T) (*fixture, *Store, *Store) {
	t.Helper()
	f, closer := closeDatabase(t)
	if _, err := f.ownerDB.Exec(f.ctx, `BEGIN;SELECT pg_advisory_xact_lock(hashtext('billing-test-role-provision'));DO $$BEGIN IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='kaana_peable_billing_reconciler') THEN CREATE ROLE kaana_peable_billing_reconciler NOLOGIN; END IF;END$$;COMMIT`); err != nil {
		t.Fatal(err)
	}
	if err := f.migrator.MigratePayments(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.migrator.MigratePayments(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.migrator.MigrateClose(f.ctx); err != nil {
		t.Fatal("close migration no longer replayable", err)
	}
	cfg := f.config.Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE kaana_peable_billing_reconciler`)
		return err
	}
	p, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return f, closer, NewStore(p)
}
func issuedForPayment(t *testing.T, f *fixture, closer *Store, amount string) Draft {
	t.Helper()
	_, _, d := delivered(t, f, amount)
	ev := closeEvidence(d)
	if _, err := closer.ClaimClose(f.ctx, d.ID, "close", ev.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := closer.RecordClose(f.ctx, d.ID, "close", ev); err != nil {
		t.Fatal(err)
	}
	return d
}
func paymentEvidence(d Draft) PaymentEvidence {
	return PaymentEvidence{InvoiceID: d.InvoiceID, Revision: strings.Repeat("a", 64), MerchantID: d.MerchantID, CustomerID: d.CustomerID, Currency: d.Currency, PaymentIntentID: "pi_" + unique(), ChargeID: "ch_" + unique(), TotalMinor: d.AmountMinor, BaseMinor: d.AmountMinor, Digest: strings.Repeat("c", 64)}
}

func TestPostgresPaymentRetiresRoundedPrincipalExactlyOnce(t *testing.T) {
	f, closer, s := paymentDatabase(t)
	d := issuedForPayment(t, f, closer, "1.235")
	ev := paymentEvidence(d)
	var beforeSpent string
	if err := f.ownerDB.QueryRow(f.ctx, `SELECT b.spent::text FROM kaana_billing.budgets b JOIN kaana_billing.accounts a ON a.active_budget_id=b.id WHERE a.id=$1`, d.AccountID).Scan(&beforeSpent); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	receipts := make(chan PaymentReceipt, 16)
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); got, err := s.RecordPayment(f.ctx, d.ID, ev); receipts <- got; errs <- err }()
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for receipt := range receipts {
		if id == "" {
			id = receipt.ID
		}
		if receipt.ID != id || receipt.Principal != "1.240000000000" {
			t.Fatal("payment duplicated or rounded differently")
		}
	}
	b, err := f.ledgerRuntime.Balance(f.ctx, d.AccountID)
	if err != nil || b.Invoiced != "0.000000000000" || b.Purchased != "0.000000000000" || b.Promotional != "0.000000000000" {
		t.Fatalf("payment balance %v %v", b, err)
	}
	var count int
	var amount, source, destination, afterSpent string
	if err = f.ownerDB.QueryRow(f.ctx, `SELECT count(*),max(amount)::text,max(source),max(destination) FROM kaana_billing.journal WHERE kind='invoice_payment'`).Scan(&count, &amount, &source, &destination); err != nil || count != 1 || amount != "1.240000000000" || source != "external_settlement" || destination != "invoice_credit" {
		t.Fatalf("payment journal %d %s %s %s %v", count, amount, source, destination, err)
	}
	if err = f.ownerDB.QueryRow(f.ctx, `SELECT b.spent::text FROM kaana_billing.budgets b JOIN kaana_billing.accounts a ON a.active_budget_id=b.id WHERE a.id=$1`, d.AccountID).Scan(&afterSpent); err != nil || beforeSpent != afterSpent {
		t.Fatal("payment reset consumed usage budget", err)
	}
	changed := ev
	changed.TotalMinor = "125"
	if _, err = s.RecordPayment(f.ctx, d.ID, changed); err == nil {
		t.Fatal("changed evidence replay accepted")
	}
}

func TestPostgresPaymentRejectsForeignEvidenceReuseAndUnissuedInvoice(t *testing.T) {
	f, closer, s := paymentDatabase(t)
	d := issuedForPayment(t, f, closer, "1.23")
	ev := paymentEvidence(d)
	for _, field := range []string{"customer", "merchant", "currency", "invoice", "revision", "base"} {
		t.Run(field, func(t *testing.T) {
			bad := ev
			switch field {
			case "customer":
				bad.CustomerID = "foreign"
			case "merchant":
				bad.MerchantID = "foreign"
			case "currency":
				bad.Currency = "EUR"
			case "invoice":
				bad.InvoiceID = "foreign"
			case "revision":
				bad.Revision = strings.Repeat("d", 64)
			case "base":
				bad.BaseMinor = "122"
			}
			if _, err := s.RecordPayment(f.ctx, d.ID, bad); err == nil {
				t.Fatal("foreign payment accepted")
			}
		})
	}
	if _, err := s.RecordPayment(f.ctx, d.ID, ev); err != nil {
		t.Fatal(err)
	}
	other := issuedForPayment(t, f, closer, "1.23")
	reused := paymentEvidence(other)
	reused.PaymentIntentID = ev.PaymentIntentID
	if _, err := s.RecordPayment(f.ctx, other.ID, reused); err == nil {
		t.Fatal("same external payment consumed twice")
	}
	b, err := f.ledgerRuntime.Balance(f.ctx, other.AccountID)
	if err != nil || b.Invoiced != "1.230000000000" {
		t.Fatal("failed duplicate changed balance", err)
	}
	_, _, unissued := delivered(t, f, "1.23")
	if _, err = s.RecordPayment(f.ctx, unissued.ID, paymentEvidence(unissued)); err == nil {
		t.Fatal("unissued invoice paid")
	}
}

func TestPostgresPaymentAtomicityAndRoleBoundary(t *testing.T) {
	f, closer, s := paymentDatabase(t)
	d := issuedForPayment(t, f, closer, "1.23")
	ev := paymentEvidence(d)
	if _, err := f.runtime.RecordPayment(f.ctx, d.ID, ev); err == nil {
		t.Fatal("delivery runtime gained payment authority")
	}
	if _, err := closer.RecordPayment(f.ctx, d.ID, ev); err == nil {
		t.Fatal("closer gained payment authority")
	}
	for _, sql := range []string{`UPDATE kaana_billing.accounts SET invoiced=0`, `DELETE FROM kaana_billing.journal`, `SELECT kaana_billing.fund('a','k','purchased','1','e')`, `SELECT kaana_peable_billing.claim_close('a','k','r')`} {
		_, err := s.pool.Exec(f.ctx, sql)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			t.Fatalf("expected actual permission denial, got %v", err)
		}
	}
	if _, err := f.ownerDB.Exec(f.ctx, `CREATE FUNCTION public.reject_invoice_payment() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.kind='invoice_payment' THEN RAISE EXCEPTION 'synthetic journal failure'; END IF;RETURN NEW;END$$;CREATE TRIGGER reject_invoice_payment BEFORE INSERT ON kaana_billing.journal FOR EACH ROW EXECUTE FUNCTION public.reject_invoice_payment()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPayment(f.ctx, d.ID, ev); err == nil {
		t.Fatal("journal failure not propagated")
	}
	b, err := f.ledgerRuntime.Balance(f.ctx, d.AccountID)
	if err != nil || b.Invoiced != "1.230000000000" {
		t.Fatal("debt committed without journal", err)
	}
	var n int
	if err = f.ownerDB.QueryRow(f.ctx, `SELECT count(*) FROM kaana_peable_billing.invoice_payments`).Scan(&n); err != nil || n != 0 {
		t.Fatal("receipt committed without journal", err)
	}
	if _, err = f.ownerDB.Exec(f.ctx, `DROP TRIGGER reject_invoice_payment ON kaana_billing.journal`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordPayment(f.ctx, d.ID, ev); err != nil {
		t.Fatal(err)
	}
	if _, err = f.ownerDB.Exec(f.ctx, `UPDATE kaana_peable_billing.invoice_payments SET principal=1`); err == nil {
		t.Fatal("payment history is mutable")
	}
	if err = s.MigratePayments(f.ctx); err == nil {
		t.Fatal("reconciler can migrate")
	}
}

func TestPostgresPaymentOperatorOwnLoginAndExcessAuthority(t *testing.T) {
	f, closer, _ := paymentDatabase(t)
	d := issuedForPayment(t, f, closer, "1.23")
	cfg, login := operatorLogin(t, f, "kaana_peable_billing_reconciler")
	s, closePool, err := openOperatorConfig(f.ctx, cfg, "reconcile-payment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordPayment(f.ctx, d.ID, paymentEvidence(d)); err != nil {
		t.Fatal(err)
	}
	closePool()
	for _, extra := range []struct{ grant, revoke string }{
		{"GRANT EXECUTE ON FUNCTION kaana_peable_billing.claim_close(text,text,text) TO " + pgx.Identifier{login}.Sanitize(), "REVOKE EXECUTE ON FUNCTION kaana_peable_billing.claim_close(text,text,text) FROM " + pgx.Identifier{login}.Sanitize()},
		{"GRANT DELETE ON kaana_billing.journal TO " + pgx.Identifier{login}.Sanitize(), "REVOKE DELETE ON kaana_billing.journal FROM " + pgx.Identifier{login}.Sanitize()},
		{"GRANT kaana_peable_billing_reconciler TO " + pgx.Identifier{login}.Sanitize() + " WITH ADMIN TRUE", "REVOKE ADMIN OPTION FOR kaana_peable_billing_reconciler FROM " + pgx.Identifier{login}.Sanitize()},
	} {
		if _, err = f.ownerDB.Exec(f.ctx, extra.grant); err != nil {
			t.Fatal(err)
		}
		_, closePool, err = openOperatorConfig(f.ctx, cfg, "reconcile-payment")
		if err == nil {
			closePool()
			t.Fatal("payment operator accepted excessive session authority")
		}
		if _, err = f.ownerDB.Exec(f.ctx, extra.revoke); err != nil {
			t.Fatal(err)
		}
		_, closePool, err = openOperatorConfig(f.ctx, cfg, "reconcile-payment")
		if err != nil {
			t.Fatal("revocation did not restore supported role", err)
		}
		closePool()
	}
}

func TestPostgresPaymentMigrationChecksums(t *testing.T) {
	f, _, _ := paymentDatabase(t)
	if _, err := f.ownerDB.Exec(f.ctx, `UPDATE public.kaana_peable_payment_migrations SET checksum='changed'`); err != nil {
		t.Fatal(err)
	}
	if err := f.migrator.MigratePayments(f.ctx); err == nil {
		t.Fatal("changed payment migration accepted")
	}
}
