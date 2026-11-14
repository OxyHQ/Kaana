package peablebilling

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type memoryClose struct {
	d          Draft
	attempt    *CloseAttempt
	loseRecord bool
}

func (s *memoryClose) Read(context.Context, string) (Draft, error) { return s.d, nil }
func (s *memoryClose) ReadClose(context.Context, string, string) (CloseAttempt, error) {
	if s.attempt == nil {
		return CloseAttempt{}, ErrCloseMissing
	}
	return *s.attempt, nil
}
func (s *memoryClose) ClaimClose(_ context.Context, id, key, rev string) (CloseClaim, error) {
	if s.attempt != nil {
		return CloseClaim{Attempt: *s.attempt}, nil
	}
	s.attempt = &CloseAttempt{ID: "kpc_test", DraftID: id, Key: key, Revision: rev, State: "attempted"}
	return CloseClaim{Fresh: true, Attempt: *s.attempt}, nil
}
func (s *memoryClose) RecordClose(_ context.Context, _, _ string, e CloseEvidence) (CloseAttempt, error) {
	if s.loseRecord {
		s.loseRecord = false
		return *s.attempt, ErrReconcile
	}
	s.attempt.State = "closed"
	s.attempt.Evidence = &e
	return *s.attempt, nil
}

type closePeable struct {
	base                        *fakePeable
	posts                       int
	lose, refuse, otherRevision bool
	requested                   bool
}

func (f *closePeable) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/finalize") {
		f.posts++
		if r.Method != http.MethodPost || r.GetBody != nil || r.Header.Get("Idempotency-Key") == "" {
			f.base.t.Fatal("unsafe finalization transport")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			f.base.t.Fatal(err)
		}
		if len(payload) != 1 || payload["expected_finalization_revision"] != strings.Repeat("a", 64) {
			f.base.t.Fatal("reviewed revision missing or extra financial instructions")
		}
		if !f.refuse {
			rev := strings.Repeat("a", 64)
			if f.otherRevision {
				rev = strings.Repeat("c", 64)
			}
			f.base.invoice["status"] = "open"
			f.base.invoice["automatic_tax"] = map[string]any{"enabled": true, "status": "complete"}
			f.base.invoice["finalization_revision"] = nil
			f.base.invoice["accepted_finalization_revision"] = rev
			f.base.invoice["number"] = "TEST-1"
			f.base.invoice["tax"] = "10"
			f.base.invoice["total"] = "134"
			f.base.invoice["amount_due"] = "134"
			f.base.invoice["ending_balance"] = "0"
		}
		if f.lose {
			f.lose = false
			return nil, ErrReconcile
		}
		if f.refuse {
			res, e := f.base.reply(map[string]any{"error": map[string]string{"code": "invoice_finalization_conflict"}})
			res.StatusCode = 409
			return res, e
		}
		return f.base.reply(f.base.invoice)
	}
	if r.URL.Path == "/v1/invoices/"+f.base.d.InvoiceID {
		if r.URL.Query().Get("include_finalization_revision") != "true" {
			f.base.t.Fatal("unreviewed retrieve")
		}
		f.requested = true
		f.base.invoice["lines"] = map[string]any{"data": []any{map[string]string{"invoice_item": f.base.d.ItemID, "amount": f.base.d.AmountMinor, "currency": f.base.d.Currency}}, "has_more": false}
		return f.base.reply(f.base.invoice)
	}
	return f.base.RoundTrip(r)
}
func closeTestClient(t *testing.T, d Draft) (*Client, *closePeable) {
	t.Helper()
	b := &fakePeable{t: t, d: d, creates: map[string]int{}}
	b.invoice = map[string]any{"id": d.InvoiceID, "object": "invoice", "customer": d.CustomerID, "currency": d.Currency, "status": "draft", "livemode": true, "auto_advance": false, "collection_method": "send_invoice", "days_until_due": d.DaysUntilDue, "automatic_tax": map[string]bool{"enabled": true}, "metadata": metadata(d), "finalization_revision": strings.Repeat("a", 64), "accepted_finalization_revision": nil, "subtotal": d.AmountMinor, "subtotal_excluding_tax": d.AmountMinor, "total_excluding_tax": d.AmountMinor, "total_discount_amounts": []any{}, "starting_balance": "0", "amount_paid": "0"}
	b.item = map[string]any{"id": d.ItemID}
	f := &closePeable{base: b}
	c := testClient(t, b)
	c.cfg.FinalizationEnabled = true
	c.http.Transport = f
	return c, f
}
func closeSample() Draft { d := sampleDraft(); d.InvoiceID = "in_own"; d.ItemID = "ii_own"; return d }
func TestCloseWorkerRecoveryNeverRepeatsFinancialPost(t *testing.T) {
	for _, loss := range []string{"", "http", "database"} {
		t.Run(loss, func(t *testing.T) {
			d := closeSample()
			s := &memoryClose{d: d, loseRecord: loss == "database"}
			c, f := closeTestClient(t, d)
			f.lose = loss == "http"
			w, e := NewCloseWorker(true, s, c)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = NewCloseWorker(false, s, c); !errors.Is(e, ErrDisabled) {
				t.Fatal("close enabled by default")
			}
			rev, e := w.Review(context.Background(), d.ID)
			if e != nil {
				t.Fatal(e)
			}
			first, e := w.Close(context.Background(), d.ID, "reviewed", rev)
			if loss != "" && e == nil {
				t.Fatal("lost response reported success")
			}
			if loss == "" && (e != nil || first.State != "closed") {
				t.Fatal(e)
			}
			restart, _ := NewCloseWorker(true, s, c)
			final, e := restart.Close(context.Background(), d.ID, "reviewed", rev)
			if e != nil || final.State != "closed" || final.Evidence == nil || final.Evidence.TaxMinor != "10" || final.Evidence.BaseMinor != "124" || f.posts != 1 || !f.requested {
				t.Fatalf("recovery: %#v %v posts=%d", final, e, f.posts)
			}
		})
	}
}
func TestCloseWorkerMissingOrDifferentProofKeepsAmbiguity(t *testing.T) {
	for _, mode := range []string{"refused", "different", "missing"} {
		t.Run(mode, func(t *testing.T) {
			d := closeSample()
			s := &memoryClose{d: d}
			c, f := closeTestClient(t, d)
			f.refuse = mode == "refused"
			f.otherRevision = mode == "different"
			f.lose = true
			w, _ := NewCloseWorker(true, s, c)
			_, _ = w.Close(context.Background(), d.ID, "key", strings.Repeat("a", 64))
			if mode == "missing" {
				delete(f.base.invoice, "accepted_finalization_revision")
			}
			for range 2 {
				if _, e := w.Close(context.Background(), d.ID, "key", strings.Repeat("a", 64)); e == nil {
					t.Fatal("unproven finalization closed")
				}
			}
			if f.posts != 1 || s.attempt.State != "attempted" {
				t.Fatal("refusal or missing proof released or replayed attempt")
			}
		})
	}
}
func TestCloseWorkerStaleReviewNeverClaimsOrPosts(t *testing.T) {
	d := closeSample()
	s := &memoryClose{d: d}
	c, f := closeTestClient(t, d)
	w, _ := NewCloseWorker(true, s, c)
	if _, e := w.Close(context.Background(), d.ID, "key", strings.Repeat("c", 64)); !errors.Is(e, ErrMismatch) {
		t.Fatal(e)
	}
	if s.attempt != nil || f.posts != 0 {
		t.Fatal("stale review acquired financial authority")
	}
}

func TestClosedEvidencePreservesInclusiveOrExclusiveAuthoritativeTax(t *testing.T) {
	for _, tc := range []struct{ net, total string }{{"124", "134"}, {"114", "124"}} {
		t.Run(tc.net, func(t *testing.T) {
			d := closeSample()
			client, remote := closeTestClient(t, d)
			remote.base.invoice["status"] = "open"
			remote.base.invoice["automatic_tax"] = map[string]any{"enabled": true, "status": "complete"}
			remote.base.invoice["number"] = "TEST-1"
			remote.base.invoice["finalization_revision"] = nil
			remote.base.invoice["accepted_finalization_revision"] = strings.Repeat("a", 64)
			remote.base.invoice["subtotal_excluding_tax"] = tc.net
			remote.base.invoice["total_excluding_tax"] = tc.net
			remote.base.invoice["tax"] = "10"
			remote.base.invoice["total"] = tc.total
			remote.base.invoice["amount_due"] = tc.total
			remote.base.invoice["ending_balance"] = "0"
			evidence, e := client.ClosedEvidence(context.Background(), d, strings.Repeat("a", 64))
			if e != nil || evidence.BaseMinor != "124" || evidence.NetBaseMinor != tc.net || evidence.TotalMinor != tc.total {
				t.Fatalf("authoritative tax changed: %#v %v", evidence, e)
			}
		})
	}
}

func TestTypedFinalizationConflictDoesNotReleaseOrReplayClose(t *testing.T) {
	d := closeSample()
	store := &memoryClose{d: d}
	client, remote := closeTestClient(t, d)
	remote.refuse = true
	worker, _ := NewCloseWorker(true, store, client)
	for range 2 {
		if _, err := worker.Close(context.Background(), d.ID, "key", strings.Repeat("a", 64)); err == nil {
			t.Fatal("typed conflict called closed")
		}
	}
	if remote.posts != 1 || store.attempt == nil || store.attempt.State != "attempted" {
		t.Fatal("conflict released claim or repeated POST")
	}
}

func TestDeliveryClientCannotFinalizeWithoutSeparateConfiguration(t *testing.T) {
	d := closeSample()
	client, remote := closeTestClient(t, d)
	client.cfg.FinalizationEnabled = false
	if err := client.Finalize(context.Background(), d, CloseAttempt{ID: "close", DraftID: d.ID, State: "attempted", Revision: strings.Repeat("a", 64)}); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := NewCloseWorker(true, &memoryClose{d: d}, client); !errors.Is(err, ErrDisabled) {
		t.Fatal("worker bypassed client opt-in")
	}
	if remote.posts != 0 {
		t.Fatal("disabled client sent financial request")
	}
}
