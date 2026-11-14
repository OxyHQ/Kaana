package peablebilling

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type paymentPeable struct {
	close          *closePeable
	intent, charge map[string]any
	calls          int
}

func (f *paymentPeable) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	if r.Method != http.MethodGet {
		f.close.base.t.Fatal("payment reconciliation attempted a remote mutation")
	}
	switch r.URL.Path {
	case "/v1/payment_intents/pi_paid":
		return f.close.base.reply(f.intent)
	case "/v1/charges/ch_paid":
		return f.close.base.reply(f.charge)
	default:
		return f.close.RoundTrip(r)
	}
}
func paymentClient(t *testing.T) (*Client, *paymentPeable, Draft, CloseAttempt) {
	t.Helper()
	d := closeSample()
	c, f := closeTestClient(t, d)
	for key, value := range map[string]any{"status": "paid", "accepted_finalization_revision": strings.Repeat("a", 64), "finalization_revision": nil, "number": "TEST-1", "tax": "10", "total": "134", "amount_due": "134", "amount_paid": "134", "amount_remaining": "0", "pre_payment_credit_notes_amount": "0", "post_payment_credit_notes_amount": "0", "payment_intent": "pi_paid", "paid_out_of_band": false, "ending_balance": "0", "automatic_tax": map[string]any{"enabled": true, "status": "complete"}} {
		f.base.invoice[key] = value
	}
	p := &paymentPeable{close: f, intent: map[string]any{"id": "pi_paid", "object": "payment_intent", "livemode": true, "merchant_id": d.MerchantID, "customer": d.CustomerID, "currency": d.Currency, "status": "succeeded", "rail": "card", "amount": "134", "amount_received": "134", "amount_refunded": "0", "amount_capturable": "0", "latest_charge": "ch_paid", "connected_account": nil, "on_behalf_of": nil, "transfer_data": nil, "client_secret": "private-payer-capability-never-persist"}, charge: map[string]any{"id": "ch_paid", "object": "charge", "livemode": true, "payment_intent": "pi_paid", "customer": d.CustomerID, "currency": d.Currency, "status": "succeeded", "amount": "134", "amount_captured": "134", "amount_refunded": "0", "paid": true, "captured": true, "refunded": false, "disputed": false}}
	c.http.Transport = p
	e := closeEvidence(d)
	e.TaxMinor = "10"
	e.TotalMinor = "134"
	e.AmountDueMinor = "134"
	return c, p, d, CloseAttempt{ID: "kpc_test", DraftID: d.ID, Key: "closed", Revision: strings.Repeat("a", 64), State: "closed", Evidence: &e}
}

type memoryPayment struct {
	d       Draft
	close   CloseAttempt
	receipt *PaymentReceipt
	lose    bool
	writes  int
}

func (s *memoryPayment) Read(context.Context, string) (Draft, error) { return s.d, nil }
func (s *memoryPayment) ReadClose(context.Context, string, string) (CloseAttempt, error) {
	return s.close, nil
}
func (s *memoryPayment) RecordPayment(_ context.Context, _ string, e PaymentEvidence) (PaymentReceipt, error) {
	if s.receipt == nil {
		s.writes++
		s.receipt = &PaymentReceipt{ID: "kpp_test", DraftID: s.d.ID, Principal: "1.240000000000", Evidence: e}
	}
	if s.lose {
		s.lose = false
		return PaymentReceipt{}, ErrReconcile
	}
	return *s.receipt, nil
}
func TestPaymentReconciliationReadOnlyAndCommitResponseLoss(t *testing.T) {
	c, remote, d, closed := paymentClient(t)
	s := &memoryPayment{d: d, close: closed, lose: true}
	if _, err := NewPaymentWorker(false, s, c); !errors.Is(err, ErrDisabled) {
		t.Fatal("payment reconciliation enabled by default")
	}
	w, _ := NewPaymentWorker(true, s, c)
	if _, err := w.Reconcile(context.Background(), d.ID, "closed"); !errors.Is(err, ErrReconcile) {
		t.Fatal("lost commit response hidden")
	}
	restarted, _ := NewPaymentWorker(true, s, c)
	got, err := restarted.Reconcile(context.Background(), d.ID, "closed")
	if err != nil || s.writes != 1 || got.Evidence.TotalMinor != "134" || got.Evidence.BaseMinor != "124" || remote.calls != 12 {
		t.Fatalf("reconciliation: %v writes %d calls %d", err, s.writes, remote.calls)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "private-payer") || strings.Contains(string(raw), "client_secret") {
		t.Fatal("payer capability persisted")
	}
}
func TestPaymentProofRefusesManualBalanceCreditRefundForeignAndMissingFacts(t *testing.T) {
	cases := []struct {
		name, target, key string
		value             any
		omit              bool
	}{
		{"manual", "invoice", "paid_out_of_band", true, false}, {"missing-manual", "invoice", "paid_out_of_band", nil, true},
		{"credit", "invoice", "post_payment_credit_notes_amount", "1", false}, {"balance", "invoice", "starting_balance", "-1", false},
		{"partial", "invoice", "amount_paid", "100", false}, {"unaccepted", "invoice", "accepted_finalization_revision", nil, false},
		{"wrong-merchant", "intent", "merchant_id", "foreign", false}, {"refunded", "intent", "amount_refunded", "1", false},
		{"other-rail", "intent", "rail", "faircoin", false}, {"delegated", "intent", "on_behalf_of", "ca_foreign", false},
		{"missing-delegation", "intent", "connected_account", nil, true}, {"uncaptured", "charge", "captured", false, false},
		{"missing-paid", "charge", "paid", nil, true}, {"disputed", "charge", "disputed", true, false},
		{"wrong-currency", "charge", "currency", "EUR", false}, {"wrong-intent", "charge", "payment_intent", "pi_other", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, remote, d, closed := paymentClient(t)
			target := remote.close.base.invoice
			switch tc.target {
			case "intent":
				target = remote.intent
			case "charge":
				target = remote.charge
			}
			if tc.omit {
				delete(target, tc.key)
			} else {
				target[tc.key] = tc.value
			}
			s := &memoryPayment{d: d, close: closed}
			w, _ := NewPaymentWorker(true, s, c)
			if _, err := w.Reconcile(context.Background(), d.ID, "closed"); err == nil || s.writes != 0 {
				t.Fatal("unproven payment retired debt")
			}
		})
	}
}
