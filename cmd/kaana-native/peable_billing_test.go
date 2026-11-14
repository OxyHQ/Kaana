package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/peablebilling"
)

type operatorFakeStore struct {
	calls []string
	draft peablebilling.Draft
	fail  bool
}

func (s *operatorFakeStore) result(call string) error {
	s.calls = append(s.calls, call)
	if s.fail {
		return errors.New("private-upstream-credential")
	}
	return nil
}
func (s *operatorFakeStore) Migrate(context.Context) error { return s.result("migrate") }
func (s *operatorFakeStore) BindCustomer(_ context.Context, a, m, c string) error {
	return s.result("bind:" + a + ":" + m + ":" + c)
}
func (s *operatorFakeStore) PrepareForMerchant(_ context.Context, m, a, k string, r []string, d int) (peablebilling.Draft, error) {
	return s.draft, s.result("prepare:" + m)
}
func (s *operatorFakeStore) Read(context.Context, string) (peablebilling.Draft, error) {
	return s.draft, s.result("read")
}
func (s *operatorFakeStore) Claim(context.Context, string, string) (bool, error) {
	return false, s.result("claim")
}
func (s *operatorFakeStore) Record(context.Context, string, string, string) error {
	return s.result("record")
}
func peableArgs(t *testing.T) []string {
	t.Helper()
	raw := []byte(`{"enabled":true,"applicationId":"app","ownerAccountId":"owner","merchantId":"merch_kaana","environment":"production"}`)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return []string{"--config-file", p, "--config-sha256", hex.EncodeToString(h[:])}
}
func TestPeableOperatorLocalActions(t *testing.T) {
	for _, tc := range []struct{ input, call string }{
		{`{"action":"migrate"}`, "migrate"}, {`{"action":"bind","accountId":"a","customerId":"c"}`, "bind:a:merch_kaana:c"}, {`{"action":"prepare","accountId":"a","key":"k","receiptIds":["r"],"daysUntilDue":30}`, "prepare:merch_kaana"}, {`{"action":"inspect","draftId":"d"}`, "read"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			store := &operatorFakeStore{draft: peablebilling.Draft{ID: "d", MerchantID: "merch_kaana"}}
			opened, closed := 0, 0
			var out bytes.Buffer
			err := runPeableBilling(peableArgs(t), strings.NewReader(tc.input), &out, func(s string) string {
				if s != "DATABASE_URL" {
					t.Fatal(s)
				}
				return "private-db"
			}, func(ctx context.Context, s string, op peablebilling.Operation) (peableOperatorStore, func(), error) {
				opened++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("no deadline")
				}
				return store, func() { closed++ }, nil
			}, func(context.Context, peablebilling.OperatorConfig) (*peablebilling.Client, error) {
				t.Fatal("local action opened AWS")
				return nil, nil
			})
			if err != nil || opened != 1 || closed != 1 || len(store.calls) != 1 || store.calls[0] != tc.call {
				t.Fatalf("err=%v open=%d close=%d calls=%v", err, opened, closed, store.calls)
			}
			if strings.Contains(out.String(), "private") || !strings.Contains(out.String(), `"complete":true`) {
				t.Fatal("bad result")
			}
		})
	}
}
func TestPeableOperatorFailBeforeConnection(t *testing.T) {
	for _, raw := range []string{`{"action":"pay","draftId":"d"}`, `{"action":"send","draftId":"d"}`, `{"action":"prepare","accountId":"a","key":"k","receiptIds":["r"]}`, `{"action":"migrate","action":"deliver","draftId":"d"}`} {
		err := runPeableBilling(peableArgs(t), strings.NewReader(raw), io.Discard, func(string) string { t.Fatal("read DB before validation"); return "" }, nil, nil)
		if err == nil {
			t.Fatal("invalid operation accepted")
		}
	}
	args := peableArgs(t)
	args[3] = strings.Repeat("0", 64)
	if err := runPeableBilling(args, strings.NewReader(`{"action":"migrate"}`), io.Discard, func(string) string { t.Fatal("read DB before review"); return "" }, nil, nil); err == nil {
		t.Fatal("changed config accepted")
	}
}
func TestPeableOperatorFailureSanitizedAndClosed(t *testing.T) {
	s := &operatorFakeStore{fail: true}
	closed := false
	var out bytes.Buffer
	err := runPeableBilling(peableArgs(t), strings.NewReader(`{"action":"migrate"}`), &out, func(string) string { return "private-db" }, func(context.Context, string, peablebilling.Operation) (peableOperatorStore, func(), error) {
		return s, func() { closed = true }, nil
	}, nil)
	if err == nil || strings.Contains(err.Error(), "private") || out.Len() != 0 || !closed {
		t.Fatal("failure leaked or cleanup missing")
	}
}

type operatorTransport func(*http.Request) (*http.Response, error)

func (f operatorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPeableOperatorDeliverReadback(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "own", true: "foreign"}[foreign], func(t *testing.T) {
			d := peablebilling.Draft{ID: "d", AccountID: "a", MerchantID: "merch_kaana", CustomerID: "c", Currency: "USD", AmountMinor: "125", DaysUntilDue: 30, InvoiceID: "in_d", ItemID: "ii_d"}
			store := &operatorFakeStore{draft: d}
			closed := false
			calls := 0
			var out bytes.Buffer
			open := func(context.Context, string, peablebilling.Operation) (peableOperatorStore, func(), error) {
				return store, func() { closed = true }, nil
			}
			clientOpen := func(_ context.Context, cfg peablebilling.OperatorConfig) (*peablebilling.Client, error) {
				return peablebilling.NewClient(cfg.ClientConfig(), operatorTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != http.MethodGet || r.URL.Host != "api.peable.to" {
						t.Fatal("unexpected request", r.Method, r.URL.Path)
					}
					var value any
					meta := map[string]string{"kaana_draft_id": "d", "kaana_account_id": "a"}
					switch r.URL.Path {
					case "/v1/merchants/me":
						app := "app"
						if foreign {
							app = "central"
						}
						value = map[string]any{"id": "merch_kaana", "object": "merchant", "oxy_app_id": app, "owner_account": "owner", "environment": "production", "livemode": true}
					case "/v1/customers/c":
						value = map[string]any{"id": "c", "object": "customer", "livemode": true, "currency": "USD", "metadata": map[string]string{"kaana_account_id": "a"}}
					case "/v1/invoices/in_d":
						value = map[string]any{"id": "in_d", "object": "invoice", "customer": "c", "currency": "USD", "status": "draft", "livemode": true, "auto_advance": false, "automatic_tax": map[string]bool{"enabled": true}, "collection_method": "send_invoice", "days_until_due": 30, "metadata": meta, "lines": map[string]any{"has_more": false, "data": []map[string]string{{"invoice_item": "ii_d", "amount": "125", "currency": "USD"}}}}
					case "/v1/invoiceitems/ii_d":
						value = map[string]any{"id": "ii_d", "object": "invoiceitem", "discountable": false, "customer": "c", "invoice": "in_d", "currency": "USD", "amount": "125", "livemode": true, "metadata": meta}
					default:
						t.Fatal("unexpected path", r.URL.Path)
					}
					raw, _ := json.Marshal(value)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
				}), func(_ context.Context, audience string) (string, error) {
					if audience != "oxy-service:6a2f851751b784a86fd0e958" {
						t.Fatal("wrong audience")
					}
					return "synthetic-token", nil
				})
			}
			err := runPeableBilling(peableArgs(t), strings.NewReader(`{"action":"deliver","draftId":"d"}`), &out, func(string) string { return "private-db" }, open, clientOpen)
			if !closed {
				t.Fatal("not closed")
			}
			if foreign {
				if err == nil || calls != 1 || out.Len() != 0 {
					t.Fatalf("foreign accepted err=%v calls=%d", err, calls)
				}
			} else {
				if err != nil || calls != 5 || !strings.Contains(out.String(), `"invoiceId":"in_d"`) {
					t.Fatalf("delivery err=%v calls=%d out=%s", err, calls, out.String())
				}
			}
			for _, call := range store.calls {
				if call != "read" {
					t.Fatal("reconciled delivery mutated", call)
				}
			}
		})
	}
}

func (s *operatorFakeStore) MigrateClose(context.Context) error { return s.result("migrate-close") }
func (s *operatorFakeStore) ReadClose(context.Context, string, string) (peablebilling.CloseAttempt, error) {
	return peablebilling.CloseAttempt{}, peablebilling.ErrCloseMissing
}
func (s *operatorFakeStore) ClaimClose(context.Context, string, string, string) (peablebilling.CloseClaim, error) {
	return peablebilling.CloseClaim{}, peablebilling.ErrDisabled
}
func (s *operatorFakeStore) RecordClose(context.Context, string, string, peablebilling.CloseEvidence) (peablebilling.CloseAttempt, error) {
	return peablebilling.CloseAttempt{}, peablebilling.ErrDisabled
}
func TestPeableCloseRequiresSeparateOptInBeforeDatabase(t *testing.T) {
	for _, operation := range []string{`{"action":"migrate-close"}`, `{"action":"review-close","draftId":"d"}`, `{"action":"close","draftId":"d","key":"k","revision":"` + strings.Repeat("a", 64) + `"}`} {
		if err := runPeableBilling(peableArgs(t), strings.NewReader(operation), io.Discard, func(string) string { t.Fatal("DB opened before separate finalization opt-in"); return "" }, nil, nil); err == nil {
			t.Fatal("close implicitly enabled")
		}
	}
	args := peableArgs(t)
	raw, err := os.ReadFile(args[1])
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"enabled":true`), []byte(`"enabled":true,"finalizationEnabled":true`), 1)
	if err = os.WriteFile(args[1], raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	args[3] = hex.EncodeToString(digest[:])
	store := &operatorFakeStore{}
	var output bytes.Buffer
	err = runPeableBilling(args, strings.NewReader(`{"action":"migrate-close"}`), &output, func(string) string { return "private-db" }, func(context.Context, string, peablebilling.Operation) (peableOperatorStore, func(), error) {
		return store, func() {}, nil
	}, func(context.Context, peablebilling.OperatorConfig) (*peablebilling.Client, error) {
		t.Fatal("migration opened identity")
		return nil, nil
	})
	if err != nil || len(store.calls) != 1 || store.calls[0] != "migrate-close" {
		t.Fatalf("migration positive control: %v %v", err, store.calls)
	}
}
