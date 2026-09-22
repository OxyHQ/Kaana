package provider_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/provider"
)

func TestCredentialClientRefusesCrossOriginRedirects(t *testing.T) {
	var destinationRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationRequests.Add(1)
	}))
	t.Cleanup(destination.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusFound)
	}))
	t.Cleanup(source.Close)

	request, err := http.NewRequest(http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer provider-secret")
	request.Header.Set("x-api-key", "provider-secret")
	response, err := provider.RefuseRedirects(nil).Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("response body close: %v", err)
		}
	})
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the original redirect", response.StatusCode)
	}
	if destinationRequests.Load() != 0 {
		t.Fatal("credential-bearing request followed a cross-origin redirect")
	}
}

/* -------------------------------------------------------------------------- */
/*  The header deadline                                                       */
/* -------------------------------------------------------------------------- */

// The four tests below are a matched set. The first states the failure the
// wrapper exists to catch; the second is its positive control, because "the
// request failed with a deadline" is also what a wrapper that failed every
// request would report. The third and fourth are the two things it must NOT
// do, and each of them is a way of being wrong that still passes the first two.

func TestAnUpstreamThatNeverSendsHeadersFailsWithADeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release // accept the request, then say nothing at all
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release) })

	client := &http.Client{Transport: provider.BoundResponseHeaders(nil, 50*time.Millisecond)}
	response, err := client.Get(upstream.URL)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("an upstream that never answered produced no error")
	}
	// The classification is the point: an adapter reads DeadlineExceeded as
	// UpstreamTimeout, which is attributable and opens the deployment's
	// breaker. Canceled would be read as the caller withdrawing and would trip
	// nothing, which is the bug this wrapper exists to avoid.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, which would be blamed on the caller rather than the provider", err)
	}
}

// TestAResponsiveUpstreamIsUntouched is the positive control for the test
// above. Without it, a BoundResponseHeaders that failed unconditionally would
// pass the whole file.
func TestAResponsiveUpstreamIsUntouched(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "answered")
	}))
	t.Cleanup(upstream.Close)

	client := &http.Client{Transport: provider.BoundResponseHeaders(nil, 50*time.Millisecond)}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("a responsive upstream failed: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("response body close: %v", err)
		}
	})
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if string(body) != "answered" {
		t.Fatalf("body = %q, want %q", body, "answered")
	}
}

// TestALongGenerationIsNotBounded is the reason this is a header deadline and
// not http.Client.Timeout. The headers arrive at once and the body then takes
// many times the timeout, which is exactly the shape of a long generation. A
// wrapper that bounded the whole exchange would pass every other test here and
// truncate real answers in production.
func TestALongGenerationIsNotBounded(t *testing.T) {
	t.Parallel()

	timeout := 50 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers now, body slowly
		for range 5 {
			time.Sleep(timeout)
			_, _ = io.WriteString(w, "chunk ")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	client := &http.Client{Transport: provider.BoundResponseHeaders(nil, timeout)}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("a slow body was refused at the headers: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("response body close: %v", err)
		}
	})

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("a body that outlived the header deadline was cut: %v", err)
	}
	if want := "chunk chunk chunk chunk chunk "; string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

// TestCallerCancellationIsStillTheCallers keeps the two blames apart. The
// wrapper owns a derived context, and a derived context is the easiest place to
// accidentally relabel the customer hanging up as a provider timeout — which
// would open a breaker on a healthy deployment every time someone closed a tab.
func TestCallerCancellationIsStillTheCallers(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	// Generous next to the cancellation, so the clock cannot be what fired.
	client := &http.Client{Transport: provider.BoundResponseHeaders(nil, time.Minute)}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("a cancelled request produced no error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, which would open a breaker on a deployment that did nothing wrong", err)
	}
}

func TestAnUnsetTimeoutLeavesTheTransportAlone(t *testing.T) {
	t.Parallel()

	base := http.DefaultTransport
	for _, timeout := range []time.Duration{0, -time.Second} {
		if got := provider.BoundResponseHeaders(base, timeout); got != base {
			t.Fatalf("BoundResponseHeaders(base, %s) wrapped the transport", timeout)
		}
	}
}
