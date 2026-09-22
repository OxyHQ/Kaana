package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// RefuseRedirects clones a client and prevents requests carrying upstream
// credentials from following a provider-controlled Location header. Go does
// not strip every authentication header on every cross-origin or downgrade
// redirect, so the only safe implicit redirect policy is none.
func RefuseRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

// DefaultResponseHeaderTimeout is how long an upstream may take to send
// response HEADERS before the attempt is abandoned.
//
// It is deliberately generous. The value has to clear the slowest legitimate
// time-to-first-byte across every provider and every model this build serves,
// including a cold deployment and a long prompt, because an attempt cut short
// here is charged to the provider's breaker. Being late is not the failure this
// bounds; never answering is.
const DefaultResponseHeaderTimeout = 90 * time.Second

// BoundResponseHeaders wraps a transport so a request fails if the upstream has
// not produced response headers within timeout. A zero or negative timeout
// returns base unchanged.
//
// # Why this is not http.Client.Timeout
//
// A client-level timeout bounds the WHOLE exchange, body included, and the body
// is the generation — so it would cut a legitimately long answer. That is why
// the adapters have always been given a client without one, and that decision
// is correct and preserved here.
//
// The reason recorded alongside it was that "the deadline belongs to the
// request context". That half does not hold: the context an adapter receives is
// the customer's own HTTP request context, and nothing ever attaches a deadline
// to it. It ends when the customer disconnects and not before. An upstream that
// completes a TCP handshake, accepts the request and then says nothing therefore
// holds a goroutine, a decrypted key pool, a breaker permit and an SSE writer
// for as long as the customer is willing to wait — and the deployment's breaker
// never opens, because no failure is ever reported for it to count.
//
// # Why not Transport.ResponseHeaderTimeout
//
// net/http honours that field on HTTP/1 connections. It is not consulted once a
// connection negotiates HTTP/2, which every provider origin this build talks to
// does, so relying on it would bound exactly the connections that are not used
// and leave the ones that are unbounded. Setting it would read as protection
// and provide none.
//
// # Why the failure is a deadline and not a cancellation
//
// The two are opposite answers about blame. An adapter classifies
// context.Canceled as the caller withdrawing — unattributable, nothing is
// retried and no breaker is touched — and context.DeadlineExceeded as
// UpstreamTimeout, which is attributable and is what opens a breaker. A wrapper
// that cancelled the context would therefore silence the very signal it exists
// to raise, so when the clock is what fired, this returns a
// context.DeadlineExceeded of its own rather than whatever the cancellation
// surfaced underneath.
func BoundResponseHeaders(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if timeout <= 0 {
		return base
	}
	return &headerDeadline{base: base, timeout: timeout}
}

type headerDeadline struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (h *headerDeadline) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(request.Context())

	// arrived guards the race between the timer firing and the headers landing.
	// Without it a response that arrived a microsecond before the deadline
	// would have its body cancelled out from under the adapter, which is a
	// truncated answer reported as a completed one.
	var mu sync.Mutex
	var arrived, expired bool
	timer := time.AfterFunc(h.timeout, func() {
		mu.Lock()
		defer mu.Unlock()
		if !arrived {
			expired = true
			cancel()
		}
	})

	response, err := h.base.RoundTrip(request.WithContext(ctx))

	mu.Lock()
	if !expired {
		arrived = true
	}
	fired := expired
	mu.Unlock()
	timer.Stop()

	if err != nil {
		cancel()
		if fired {
			return nil, fmt.Errorf("provider: no response headers within %s: %w", h.timeout, context.DeadlineExceeded)
		}
		return nil, err
	}
	if fired {
		// The clock won, but the transport still handed back a response. Close
		// it rather than leaking the connection, and report the deadline.
		_ = response.Body.Close()
		cancel()
		return nil, fmt.Errorf("provider: no response headers within %s: %w", h.timeout, context.DeadlineExceeded)
	}

	// The headers arrived, so the clock stops and the BODY is left unbounded:
	// that is the generation, and the customer's own context is what ends it.
	// cancel is owed to the context either way, so it rides on Close.
	response.Body = &cancelOnClose{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// cancelOnClose releases the request context when the body is closed. Every
// path that reads an upstream response closes it — the conformance suite has a
// control for the ones that fail midway — so this is where the context's
// lifetime ends without bounding the read.
type cancelOnClose struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}
