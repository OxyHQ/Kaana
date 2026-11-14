package peablebilling

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func closeDatabase(t *testing.T) (*fixture, *Store) {
	t.Helper()
	f := database(t)
	tx, e := f.ownerDB.Begin(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-test-role-provision')); DO $$BEGIN IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname='kaana_peable_billing_closer') THEN CREATE ROLE kaana_peable_billing_closer NOLOGIN; END IF; END$$`); e != nil {
		t.Fatal(e)
	}
	if e := tx.Commit(f.ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.migrator.MigrateClose(f.ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.migrator.MigrateClose(f.ctx); e != nil {
		t.Fatal(e)
	}
	// The old draft migration remains independently replayable.
	if e := f.migrator.Migrate(f.ctx); e != nil {
		t.Fatal(e)
	}
	cfg := f.config.Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE kaana_peable_billing_closer`)
		return err
	}
	p, e := pgxpool.NewWithConfig(f.ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.Close)
	return f, NewStore(p)
}
func delivered(t *testing.T, f *fixture, amount string) (billing.Account, billing.Receipt, Draft) {
	t.Helper()
	a := f.account(t)
	r := f.receipt(t, a, amount)
	d, e := f.admin.Prepare(f.ctx, a.ID, unique(), []string{r.ID}, 30)
	if e != nil {
		t.Fatal(e)
	}
	for _, step := range []string{"invoice", "item"} {
		if _, e := f.runtime.Claim(f.ctx, d.ID, step); e != nil {
			t.Fatal(e)
		}
		if e := f.runtime.Record(f.ctx, d.ID, step, step+"_"+unique()); e != nil {
			t.Fatal(e)
		}
	}
	d, e = f.runtime.Read(f.ctx, d.ID)
	if e != nil {
		t.Fatal(e)
	}
	return a, r, d
}
func closeEvidence(d Draft) CloseEvidence {
	return CloseEvidence{NetBaseMinor: d.AmountMinor, StartingBalanceMinor: "0", EndingBalanceMinor: "0", AmountDueMinor: d.AmountMinor, AmountPaidMinor: "0", InvoiceID: d.InvoiceID, Revision: strings.Repeat("a", 64), Status: "open", Number: "TEST-1", Digest: strings.Repeat("b", 64), BaseMinor: d.AmountMinor, TaxMinor: "0", TotalMinor: d.AmountMinor}
}
func TestPostgresCloseRoundsOnceWithoutMarkingDebtPaid(t *testing.T) {
	for _, tc := range []struct{ amount, want, source, destination string }{{"1.234", "1.230000000000", "revenue", "invoice_credit"}, {"1.235", "1.240000000000", "invoice_credit", "revenue"}, {"1.230", "1.230000000000", "", ""}} {
		t.Run(tc.amount, func(t *testing.T) {
			f, s := closeDatabase(t)
			a, r, d := delivered(t, f, tc.amount)
			ev := closeEvidence(d)
			claim, e := s.ClaimClose(f.ctx, d.ID, "close", ev.Revision)
			if e != nil || !claim.Fresh {
				t.Fatalf("claim: %v", e)
			}
			again, e := s.ClaimClose(f.ctx, d.ID, "close", ev.Revision)
			if e != nil || again.Fresh || again.Attempt.ID != claim.Attempt.ID {
				t.Fatalf("claim replay: %v", e)
			}
			if _, e := s.ClaimClose(f.ctx, d.ID, "different", ev.Revision); e == nil {
				t.Fatal("ambiguous claim evaded with new key")
			}
			refund := billing.RefundRequest{AccountID: a.ID, ReceiptID: r.ID, Key: unique(), Amount: "0.1", EvidenceID: unique()}
			if _, e := f.ledgerAdmin.Refund(f.ctx, refund); e == nil {
				t.Fatal("refund crossed ambiguous finalization")
			}
			closed, e := s.RecordClose(f.ctx, d.ID, "close", ev)
			if e != nil || closed.State != "closed" {
				t.Fatalf("record: %v", e)
			}
			if _, e := s.RecordClose(f.ctx, d.ID, "close", ev); e != nil {
				t.Fatal(e)
			}
			var debt string
			var count int
			if e := f.ownerDB.QueryRow(f.ctx, `SELECT invoiced::text FROM kaana_billing.accounts WHERE id=$1`, a.ID).Scan(&debt); e != nil {
				t.Fatal(e)
			}
			if debt != tc.want {
				t.Fatalf("debt %s, want %s", debt, tc.want)
			}
			if e := f.ownerDB.QueryRow(f.ctx, `SELECT count(*) FROM kaana_billing.journal WHERE operation_id=$1`, claim.Attempt.ID).Scan(&count); e != nil {
				t.Fatal(e)
			}
			if tc.source == "" {
				if count != 0 {
					t.Fatal("zero adjustment posted")
				}
			} else {
				var src, dst string
				if count != 1 {
					t.Fatalf("journal count %d", count)
				}
				if e := f.ownerDB.QueryRow(f.ctx, `SELECT source,destination FROM kaana_billing.journal WHERE operation_id=$1`, claim.Attempt.ID).Scan(&src, &dst); e != nil {
					t.Fatal(e)
				}
				if src != tc.source || dst != tc.destination {
					t.Fatal("incorrect balanced rounding direction")
				}
			}
			if _, e := f.ledgerAdmin.Refund(f.ctx, refund); e == nil {
				t.Fatal("issued invoice bypassed compensation reconciliation")
			}
			ev.TaxMinor = "1"
			ev.TotalMinor = "125"
			if _, e := s.RecordClose(f.ctx, d.ID, "close", ev); e == nil {
				t.Fatal("changed evidence replay accepted")
			}
		})
	}
}
func TestPostgresCloseChangedReceiptNetRefusesBeforeAttempt(t *testing.T) {
	f, s := closeDatabase(t)
	a, r, d := delivered(t, f, "2.235")
	if _, e := f.ledgerAdmin.Refund(f.ctx, billing.RefundRequest{AccountID: a.ID, ReceiptID: r.ID, Key: unique(), Amount: "0.1", EvidenceID: unique()}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ClaimClose(f.ctx, d.ID, "close", strings.Repeat("a", 64)); e == nil {
		t.Fatal("changed receipt net accepted")
	}
}
func TestPostgresCloseInvalidEvidenceAndLeastPrivilege(t *testing.T) {
	f, s := closeDatabase(t)
	_, _, d := delivered(t, f, "1.234")
	ev := closeEvidence(d)
	if _, e := s.ClaimClose(f.ctx, d.ID, "close", ev.Revision); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*CloseEvidence){func(e *CloseEvidence) { e.Revision = strings.Repeat("c", 64) }, func(e *CloseEvidence) { e.BaseMinor = "124"; e.TotalMinor = "124" }, func(e *CloseEvidence) { e.InvoiceID = "foreign" }, func(e *CloseEvidence) { e.Status = "draft" }, func(e *CloseEvidence) { e.Digest = "" }, func(e *CloseEvidence) { e.StartingBalanceMinor = "-0" }} {
		bad := ev
		change(&bad)
		if _, e := s.RecordClose(f.ctx, d.ID, "close", bad); e == nil {
			t.Fatal("invalid evidence accepted")
		}
	}
	raw, _ := json.Marshal(ev)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	obj["number"] = nil
	raw, _ = json.Marshal(obj)
	if _, e := s.pool.Exec(f.ctx, `SELECT kaana_peable_billing.record_close($1,$2,$3::jsonb)`, d.ID, "close", raw); e == nil {
		t.Fatal("null evidence accepted")
	}
	if _, e := f.runtime.ClaimClose(f.ctx, d.ID, "bad", ev.Revision); e == nil {
		t.Fatal("delivery obtained close authority")
	}
	if _, e := s.Prepare(f.ctx, d.AccountID, "bad", []string{"receipt"}, 30); e == nil {
		t.Fatal("closer obtained preparation authority")
	}
	if _, e := s.pool.Exec(f.ctx, `UPDATE kaana_billing.accounts SET invoiced=0`); e == nil {
		t.Fatal("closer direct financial DML")
	}
	if e := s.MigrateClose(f.ctx); e == nil {
		t.Fatal("closer migrated")
	}
	if _, e := s.RecordClose(f.ctx, d.ID, "close", ev); e != nil {
		t.Fatal(e)
	}
}
func TestPostgresCloseRefundRaceHasOneFinancialWinner(t *testing.T) {
	f, s := closeDatabase(t)
	a, r, d := delivered(t, f, "1.234")
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var closeErr, refundErr error
	go func() {
		defer wg.Done()
		<-start
		_, closeErr = s.ClaimClose(f.ctx, d.ID, "close", strings.Repeat("a", 64))
	}()
	go func() {
		defer wg.Done()
		<-start
		_, refundErr = f.ledgerAdmin.Refund(f.ctx, billing.RefundRequest{AccountID: a.ID, ReceiptID: r.ID, Key: unique(), Amount: "0.1", EvidenceID: unique()})
	}()
	close(start)
	wg.Wait()
	if (closeErr == nil) == (refundErr == nil) {
		t.Fatalf("exactly one must commit: close=%v refund=%v", closeErr, refundErr)
	}
}

func TestPostgresCloseWorkerRestartUsesDurableAcceptedProof(t *testing.T) {
	f, s := closeDatabase(t)
	a, _, d := delivered(t, f, "1.235")
	client, remote := closeTestClient(t, d)
	remote.lose = true
	worker, e := NewCloseWorker(true, s, client)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = worker.Close(f.ctx, d.ID, "reviewed", strings.Repeat("a", 64)); e == nil {
		t.Fatal("lost response claimed success")
	}
	attempt, e := s.ReadClose(f.ctx, d.ID, "reviewed")
	if e != nil || attempt.State != "attempted" {
		t.Fatalf("durable attempt missing %v", e)
	}
	restart, e := NewCloseWorker(true, NewStore(s.pool), client)
	if e != nil {
		t.Fatal(e)
	}
	result, e := restart.Close(f.ctx, d.ID, "reviewed", strings.Repeat("a", 64))
	if e != nil || result.State != "closed" || remote.posts != 1 {
		t.Fatalf("recovery failed %v", e)
	}
	if _, e = restart.Close(f.ctx, d.ID, "reviewed", strings.Repeat("a", 64)); e != nil {
		t.Fatal(e)
	}
	var debt string
	var count int
	if e = f.ownerDB.QueryRow(f.ctx, `SELECT invoiced::text FROM kaana_billing.accounts WHERE id=$1`, a.ID).Scan(&debt); e != nil {
		t.Fatal(e)
	}
	if e = f.ownerDB.QueryRow(f.ctx, `SELECT count(*) FROM kaana_billing.journal WHERE operation_id=$1`, result.ID).Scan(&count); e != nil {
		t.Fatal(e)
	}
	if debt != "1.240000000000" || count != 1 || remote.posts != 1 {
		t.Fatal("issue was collected or rounding/retry duplicated")
	}
}

func TestPostgresCloseOperatorHasOnlyReviewedCloseAuthority(t *testing.T) {
	f, _ := closeDatabase(t)
	_, _, d := delivered(t, f, "1.235")
	cfg, login := operatorLogin(t, f, "kaana_peable_billing_closer")
	s, closePool, e := openOperatorConfig(f.ctx, cfg, "close")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ClaimClose(f.ctx, d.ID, "reviewed", strings.Repeat("a", 64)); e != nil {
		t.Fatal(e)
	}
	closePool()
	for _, authority := range []struct{ grant, revoke string }{
		{"GRANT EXECUTE ON FUNCTION kaana_peable_billing.prepare_draft(text,text,text[],integer) TO ", "REVOKE EXECUTE ON FUNCTION kaana_peable_billing.prepare_draft(text,text,text[],integer) FROM "},
		{"GRANT DELETE ON kaana_billing.accounts TO ", "REVOKE DELETE ON kaana_billing.accounts FROM "},
	} {
		if _, e = f.ownerDB.Exec(f.ctx, authority.grant+pgx.Identifier{login}.Sanitize()); e != nil {
			t.Fatal(e)
		}
		if _, closer, err := openOperatorConfig(f.ctx, cfg, "close"); err == nil {
			closer()
			t.Fatal("excess session authority accepted")
		}
		if _, e = f.ownerDB.Exec(f.ctx, authority.revoke+pgx.Identifier{login}.Sanitize()); e != nil {
			t.Fatal(e)
		}
		_, closer, err := openOperatorConfig(f.ctx, cfg, "close")
		if err != nil {
			t.Fatal(err)
		}
		closer()
	}
	if _, closer, err := openOperatorConfig(f.ctx, cfg, "deliver"); err == nil {
		closer()
		t.Fatal("closer could create invoices")
	}
}

func TestPostgresClosePreservesPeableInclusiveTaxWithoutInventingFunds(t *testing.T) {
	f, s := closeDatabase(t)
	a, _, d := delivered(t, f, "1.235")
	proof := closeEvidence(d)
	proof.NetBaseMinor = "114"
	proof.TaxMinor = "10"
	proof.TotalMinor = "124"
	if _, e := s.ClaimClose(f.ctx, d.ID, "inclusive", proof.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e := s.RecordClose(f.ctx, d.ID, "inclusive", proof); e != nil {
		t.Fatal(e)
	}
	var debt string
	if e := f.ownerDB.QueryRow(f.ctx, `SELECT invoiced::text FROM kaana_billing.accounts WHERE id=$1`, a.ID).Scan(&debt); e != nil {
		t.Fatal(e)
	}
	if debt != "1.240000000000" {
		t.Fatal("inclusive tax altered settled base or created funds")
	}
}

func TestPostgresCloseMigrationRequiresActualOwnerAndChecksChecksum(t *testing.T) {
	f, _ := closeDatabase(t)
	cfg, _ := operatorLogin(t, f, "kaana_billing_migrator")
	store, closePool, e := openOperatorConfig(f.ctx, cfg, "migrate-close")
	if e != nil {
		t.Fatal(e)
	}
	defer closePool()
	if e = store.MigrateClose(f.ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = f.ownerDB.Exec(f.ctx, `UPDATE public.kaana_peable_close_migrations SET checksum='changed'`); e != nil {
		t.Fatal(e)
	}
	if e = store.MigrateClose(f.ctx); e == nil {
		t.Fatal("changed migration accepted")
	}
}
