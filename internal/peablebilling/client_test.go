package peablebilling

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type memoryStore struct {
	d          Draft
	failRecord bool
}

func (s *memoryStore) Read(context.Context, string) (Draft, error) { return s.d, nil }
func (s *memoryStore) Claim(_ context.Context, _ string, step string) (bool, error) {
	if step == "invoice" {
		fresh := !s.d.InvoiceAttempted
		s.d.InvoiceAttempted = true
		return fresh, nil
	}
	fresh := !s.d.ItemAttempted
	s.d.ItemAttempted = true
	return fresh, nil
}
func (s *memoryStore) Record(_ context.Context, _ string, step, id string) error {
	if s.failRecord {
		s.failRecord = false
		return errors.New("injected durable response failure")
	}
	if step == "invoice" {
		s.d.InvoiceID = id
	} else {
		s.d.ItemID = id
	}
	return nil
}
func sampleDraft() Draft {
	return Draft{ID: "kpd_reviewed", AccountID: "kba_owner", MerchantID: "merch_kaana", CustomerID: "cus_owner", Currency: "USD", AmountMinor: "124", Subtotal: "1.235", RoundingAdjustment: "0.005", MinorExponent: 2, DaysUntilDue: 30}
}

type fakePeable struct {
	t             *testing.T
	d             Draft
	invoice, item map[string]any
	creates       map[string]int
	lose          string
	foreign       bool
	extraLine     bool
	eventType     string
	noCreate      bool
	bodyChecks    int
}

func (f *fakePeable) reply(value any) (*http.Response, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
}
func (f *fakePeable) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	if r.URL.Scheme != "https" || r.URL.Host != "api.peable.to" || r.Header.Get("Peable-Version") != "2026-11-03" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		f.t.Fatal("noncanonical Peable transport")
	}
	path := r.URL.Path
	if path == "/v1/merchants/me" {
		app := "own-app"
		if f.foreign {
			app = "central-app"
		}
		return f.reply(map[string]any{"id": f.d.MerchantID, "object": "merchant", "oxy_app_id": app, "owner_account": "own-org", "environment": "production", "livemode": true})
	}
	if strings.HasPrefix(path, "/v1/customers/") {
		return f.reply(map[string]any{"id": f.d.CustomerID, "object": "customer", "livemode": true, "currency": f.d.Currency, "metadata": map[string]string{"kaana_account_id": f.d.AccountID}})
	}
	if strings.HasPrefix(path, "/v1/events/") {
		return f.reply(map[string]any{"id": "evt_own", "object": "event", "type": f.eventType, "livemode": true, "data": map[string]any{"object": map[string]any{"object": "invoice", "metadata": metadata(f.d)}}})
	}
	if r.Method == http.MethodPost {
		if r.GetBody != nil {
			f.t.Fatal("transport may transparently replay financial POST")
		}
		if path != "/v1/invoices" && path != "/v1/invoiceitems" {
			f.t.Fatal("financial action outside draft slice")
		}
		f.creates[path]++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Fatal(err)
		}
		if body["customer"] != f.d.CustomerID || body["currency"] != f.d.Currency || r.Header.Get("Idempotency-Key") == "" {
			f.t.Fatal("request ownership or idempotency missing")
		}
		if path == "/v1/invoices" {
			if body["auto_advance"] != false || body["collection_method"] != "send_invoice" || body["pending_invoice_items_behavior"] != "exclude" || body["days_until_due"] != float64(f.d.DaysUntilDue) || body["automatic_tax"].(map[string]any)["enabled"] != true {
				f.t.Fatal("draft billing terms changed")
			}
			if !f.noCreate {
				f.invoice = map[string]any{"id": "in_own", "object": "invoice", "customer": f.d.CustomerID, "currency": f.d.Currency, "status": "draft", "livemode": true, "auto_advance": false, "collection_method": "send_invoice", "days_until_due": f.d.DaysUntilDue, "automatic_tax": map[string]bool{"enabled": true}, "metadata": metadata(f.d)}
			}
		} else {
			if body["invoice"] != "in_own" || body["amount"] != f.d.AmountMinor || body["discountable"] != false {
				f.t.Fatal("item amount or ownership changed")
			}
			if !f.noCreate {
				f.item = map[string]any{"id": "ii_own", "object": "invoiceitem", "customer": f.d.CustomerID, "invoice": "in_own", "currency": f.d.Currency, "amount": f.d.AmountMinor, "livemode": true, "discountable": false, "metadata": metadata(f.d)}
			}
		}
		f.bodyChecks++
		if f.lose == path {
			f.lose = ""
			return nil, errors.New("injected lost response")
		}
		if path == "/v1/invoices" {
			return f.reply(f.invoice)
		}
		return f.reply(f.item)
	}
	if path == "/v1/invoices" || path == "/v1/invoiceitems" {
		if r.URL.Query().Get("customer") != f.d.CustomerID {
			f.t.Fatal("unscoped reconciliation")
		}
		data := []any{}
		if path == "/v1/invoices" && f.invoice != nil {
			data = append(data, f.invoice)
		}
		if path == "/v1/invoiceitems" && f.item != nil {
			data = append(data, f.item)
		}
		return f.reply(map[string]any{"object": "list", "data": data, "has_more": false})
	}
	if path == "/v1/invoices/in_own" {
		lines := []any{}
		if f.item != nil {
			lines = append(lines, map[string]any{"invoice_item": "ii_own", "amount": f.d.AmountMinor, "currency": f.d.Currency})
		}
		if f.extraLine {
			lines = append(lines, map[string]any{"invoice_item": "ii_foreign", "amount": "99", "currency": f.d.Currency})
		}
		f.invoice["lines"] = map[string]any{"data": lines, "has_more": false}
		return f.reply(f.invoice)
	}
	if path == "/v1/invoiceitems/ii_own" {
		return f.reply(f.item)
	}
	f.t.Fatal("unexpected request")
	return nil, ErrMismatch
}
func testClient(t *testing.T, f *fakePeable) *Client {
	t.Helper()
	token := unique()
	c, err := NewClient(Config{Enabled: true, ApplicationID: "own-app", OwnerAccountID: "own-org", MerchantID: f.d.MerchantID, Environment: "production"}, f, func(_ context.Context, audience string) (string, error) {
		if audience != peableAudience {
			t.Fatal("wrong workload audience")
		}
		return token, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestWorkerDraftDeliveryAndLostResponses(t *testing.T) {
	for _, loss := range []string{"", "/v1/invoices", "/v1/invoiceitems", "database"} {
		t.Run(loss, func(t *testing.T) {
			d := sampleDraft()
			s := &memoryStore{d: d, failRecord: loss == "database"}
			f := &fakePeable{t: t, d: d, creates: map[string]int{}, lose: loss, eventType: "invoice.updated"}
			c := testClient(t, f)
			w, err := NewWorker(s, c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.Deliver(context.Background(), d.ID)
			if loss != "" && err == nil {
				t.Fatal("ambiguous result was called success")
			}
			if loss == "" && err != nil {
				t.Fatal(err)
			}
			// New worker after process restart must reconcile a persisted attempted step.
			w, err = NewWorker(s, c)
			if err != nil {
				t.Fatal(err)
			}
			out, err := w.Deliver(context.Background(), d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if out.InvoiceID != "in_own" || out.ItemID != "ii_own" || f.creates["/v1/invoices"] != 1 || f.creates["/v1/invoiceitems"] != 1 || f.bodyChecks != 2 {
				t.Fatalf("not exactly one draft/item: %#v", f.creates)
			}
			if _, err = w.ReconcileEvent(context.Background(), "evt_own"); err != nil {
				t.Fatal(err)
			}
			if f.creates["/v1/invoices"] != 1 || f.creates["/v1/invoiceitems"] != 1 {
				t.Fatal("duplicate event created objects")
			}
		})
	}
}
func TestUnknownOutcomeNeverCreatesAgain(t *testing.T) {
	d := sampleDraft()
	s := &memoryStore{d: d}
	f := &fakePeable{t: t, d: d, creates: map[string]int{}, lose: "/v1/invoices", noCreate: true}
	w, e := NewWorker(s, testClient(t, f))
	if e != nil {
		t.Fatal(e)
	}
	for range 3 {
		if _, err := w.Deliver(context.Background(), d.ID); !errors.Is(err, ErrReconcile) {
			t.Fatalf("unknown outcome: %v", err)
		}
	}
	if f.creates["/v1/invoices"] != 1 || f.creates["/v1/invoiceitems"] != 0 {
		t.Fatal("ambiguous POST reissued")
	}
}
func TestForeignMerchantAndManualInvoiceChangesRefused(t *testing.T) {
	d := sampleDraft()
	s := &memoryStore{d: d}
	f := &fakePeable{t: t, d: d, creates: map[string]int{}, foreign: true}
	w, e := NewWorker(s, testClient(t, f))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Deliver(context.Background(), d.ID); !errors.Is(e, ErrMismatch) || len(f.creates) != 0 {
		t.Fatal("central/foreign merchant accepted")
	}
	f.foreign = false
	f.extraLine = true
	if _, e = w.Deliver(context.Background(), d.ID); !errors.Is(e, ErrMismatch) {
		t.Fatal("unexpected invoice lines accepted")
	}
	f.extraLine = false
	f.invoice["status"] = "paid"
	if _, e = w.Deliver(context.Background(), d.ID); !errors.Is(e, ErrMismatch) {
		t.Fatal("paid invoice treated as editable draft")
	}
	f.eventType = "invoice.paid"
	if _, e = w.ReconcileEvent(context.Background(), "evt_own"); !errors.Is(e, ErrMismatch) {
		t.Fatal("paid event used for draft/funding action")
	}
}
func TestDefaultDisabledAndRedirectRefusal(t *testing.T) {
	if _, err := NewClient(Config{}, nil, nil); !errors.Is(err, ErrDisabled) {
		t.Fatal("client enabled by default")
	}
	f := &fakePeable{t: t, d: sampleDraft()}
	c := testClient(t, f)
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://elsewhere.invalid"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := c.Verify(context.Background(), f.d); !errors.Is(err, ErrReconcile) {
		t.Fatal("unexpected redirect accepted")
	}
}
func TestPostgresWorkerReconcilesAfterLostCreate(t *testing.T) {
	f := database(t)
	a := f.account(t)
	r := f.receipt(t, a, "1.235")
	d, e := f.admin.Prepare(f.ctx, a.ID, "worker", []string{r.ID}, 30)
	if e != nil {
		t.Fatal(e)
	}
	remote := &fakePeable{t: t, d: d, creates: map[string]int{}, lose: "/v1/invoices"}
	w, e := NewWorker(f.runtime, testClient(t, remote))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Deliver(f.ctx, d.ID); !errors.Is(e, ErrReconcile) {
		t.Fatal("lost response not preserved")
	}
	w, e = NewWorker(NewStore(f.runtimeDB), testClient(t, remote))
	if e != nil {
		t.Fatal(e)
	}
	out, e := w.Deliver(f.ctx, d.ID)
	if e != nil {
		t.Fatal(e)
	}
	if out.InvoiceID != "in_own" || out.ItemID != "ii_own" || remote.creates["/v1/invoices"] != 1 {
		t.Fatal("durable recovery duplicated draft")
	}
	balance, e := f.ledgerRuntime.Balance(f.ctx, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if balance.Invoiced != "1.235000000000" {
		t.Fatal("draft delivery changed invoice credit balance")
	}
}
