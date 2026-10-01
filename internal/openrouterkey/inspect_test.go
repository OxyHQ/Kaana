package openrouterkey

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const fixture = `{"data":{"organization_id":"org_fixture","workspace_id":"workspace_fixture","limit":null,"limit_remaining":null,"limit_reset":null,"usage":0.624,"usage_daily":0,"usage_weekly":0,"usage_monthly":0.624,"is_free_tier":false,"expires_at":null,"label":"SYNTHETIC_SECRET_LABEL","unknown":{"nested":true}}}`

func inspectFixture(t *testing.T, body string, status int, header string) (Report, int, error) {
	t.Helper()
	calls := 0
	inspector, err := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != Endpoint || r.Method != http.MethodGet || r.Body != nil || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Fatal("request boundary violated")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("deadline absent")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {header}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	report, err := inspector.Inspect(context.Background(), []byte("synthetic-key"), Expectation{OrganizationID: "org_fixture", WorkspaceID: "workspace_fixture"})
	return report, calls, err
}
func TestExactReadAndProjection(t *testing.T) {
	report, calls, err := inspectFixture(t, fixture, 200, "application/json; charset=utf-8")
	if err != nil || calls != 1 || report.Verdict() != nil || report.UsageUSD != 0.624 {
		t.Fatalf("read: %+v %v calls=%d", report, err, calls)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"SYNTHETIC_SECRET_LABEL", "org_fixture", "workspace_fixture", "nested", "synthetic-key"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatal("unsafe output projection")
		}
	}
}
func TestOwnershipFailsClosed(t *testing.T) {
	for _, replacement := range []string{`"organization_id":null`, `"organization_id":"different"`, `"unused":null`} {
		report, calls, err := inspectFixture(t, strings.Replace(fixture, `"organization_id":"org_fixture"`, replacement, 1), 200, "application/json")
		if err != nil || calls != 1 || !errors.Is(report.Verdict(), ErrExpectationNotMet) {
			t.Fatalf("ownership boundary %s: %v", replacement, err)
		}
	}
	report, _, err := inspectFixture(t, strings.Replace(fixture, `"workspace_id":"workspace_fixture"`, `"workspace_id":null`, 1), 200, "application/json")
	if err != nil || !errors.Is(report.Verdict(), ErrExpectationNotMet) {
		t.Fatal("workspace absence accepted")
	}
}
func TestMalformedBoundaries(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate":        strings.Replace(fixture, `"usage":0.624`, `"usage":0.624,"usage":0`, 1),
		"case":             strings.Replace(fixture, `"usage":0.624`, `"Usage":0.624`, 1),
		"wrong-owner-type": strings.Replace(fixture, `"organization_id":"org_fixture"`, `"organization_id":123`, 1),
		"negative":         strings.Replace(fixture, `"usage":0.624`, `"usage":-1`, 1),
		"overflow":         strings.Replace(fixture, `"usage":0.624`, `"usage":1e999`, 1),
		"string-number":    strings.Replace(fixture, `"usage":0.624`, `"usage":"0.624"`, 1),
		"missing":          strings.Replace(fixture, `"usage":0.624,`, "", 1),
		"trailing":         fixture + `{}`, "null-data": `{"data":null}`,
		"boolean":   strings.Replace(fixture, `"is_free_tier":false`, `"is_free_tier":null`, 1),
		"timestamp": strings.Replace(fixture, `"expires_at":null`, `"expires_at":"bad"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, calls, err := inspectFixture(t, body, 200, "application/json")
			if !errors.Is(err, ErrMalformedResponse) || calls != 1 {
				t.Fatalf("malformed accepted: %v", err)
			}
		})
	}
	_, _, err := inspectFixture(t, strings.Repeat("x", MaxResponseBytes+1), 200, "application/json")
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatal(err)
	}
	_, _, err = inspectFixture(t, fixture, 200, "text/plain")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatal(err)
	}
}
func TestNoRedirectRetryOrErrorLeak(t *testing.T) {
	for status, want := range map[int]error{302: ErrRedirectRefused, 401: ErrCredentialRejected, 403: ErrUnexpectedStatus, 429: ErrUnexpectedStatus, 500: ErrUnexpectedStatus} {
		_, calls, err := inspectFixture(t, "synthetic-key", status, "application/json")
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("status %d: %v calls=%d", status, err, calls)
		}
	}
	calls := 0
	inspector, _ := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("synthetic-key secret transport")
	}))
	_, err := inspector.Inspect(context.Background(), []byte("synthetic-key"), Expectation{OrganizationID: "org_fixture"})
	if !errors.Is(err, ErrRequestFailed) || calls != 1 || strings.Contains(err.Error(), "synthetic-key") {
		t.Fatal("transport error leak or retry")
	}
	calls = 0
	for _, expect := range []Expectation{{}, {OrganizationID: "bad\n"}} {
		_, err = inspector.Inspect(context.Background(), []byte("synthetic-key"), expect)
		if !errors.Is(err, ErrInvalidExpectation) {
			t.Fatal(err)
		}
	}
	_, err = inspector.Inspect(context.Background(), []byte("bad\n"), Expectation{OrganizationID: "org_fixture"})
	if !errors.Is(err, ErrInvalidCredential) || calls != 0 {
		t.Fatal("invalid input reached transport")
	}
	production := NewProduction().client.Transport.(*http.Transport)
	if production.Proxy != nil || !production.DisableKeepAlives || production.Protocols == nil || !production.Protocols.HTTP1() || production.Protocols.HTTP2() || production.ForceAttemptHTTP2 {
		t.Fatal("production transport can proxy or replay")
	}
}

type deadlineBody struct{ ctx context.Context }

func (b deadlineBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (deadlineBody) Close() error               { return nil }
func TestCancelledContextAndBodyDeadline(t *testing.T) {
	calls := 0
	inspector, _ := New(roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, r.Context().Err() }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := inspector.Inspect(ctx, []byte("synthetic-key"), Expectation{OrganizationID: "org_fixture"})
	if !errors.Is(err, ErrRequestFailed) || calls > 1 {
		t.Fatalf("cancel: %v calls=%d", err, calls)
	}
	inspector, _ = New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: deadlineBody{r.Context()}, Request: r}, nil
	}))
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err = inspector.Inspect(ctx, []byte("synthetic-key"), Expectation{OrganizationID: "org_fixture"})
	if !errors.Is(err, ErrTimeout) {
		t.Fatal("body deadline not respected", err)
	}
}
