// Package requesttiming measures where one public request's time goes: the
// grant from Oxy, each upstream exchange (connection, time to first byte,
// body), the evidence Kaana writes on the hot path and the waits between
// retries. It records durations, booleans and status codes only: never a
// header, a URL, a body, a credential or anything a caller sent, so a
// recording can be logged whole (docs/rules.md, "Secrets and customer data").
package requesttiming

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Peer names who an exchange was with. It is a closed set so a log line can
// never carry a hostname.
type Peer string

const (
	// PeerOxy is Oxy's authorization API (the grant).
	PeerOxy Peer = "oxy"
	// PeerProvider is an upstream provider.
	PeerProvider Peer = "provider"
)

// maxExchanges bounds what one request records. A request makes at most a few
// upstream exchanges (RetryPolicy, the key walk); the bound keeps a
// pathological one from growing a log line without limit.
const maxExchanges = 8

// Exchange is one HTTP round trip, measured from the moment it was handed to
// the transport.
type Exchange struct {
	Peer Peer
	// Reused reports that the exchange ran on a kept-alive connection, so it
	// paid no DNS, TCP or TLS cost.
	Reused bool
	// DNS, Connect and TLS are zero on a reused connection.
	DNS, Connect, TLS time.Duration
	// FirstByte is the time to the first response byte; Headers the time
	// until the response headers were parsed; Done the time until the body
	// was closed (or the exchange failed).
	FirstByte, Headers, Done time.Duration
	// Status is the HTTP status; zero when no response arrived.
	Status int
}

// Request accumulates one request's measurements. Its methods are safe for
// concurrent use and are no-ops on a nil receiver, so a caller that is not
// being measured needs no branch.
type Request struct {
	mu                sync.Mutex
	exchanges         []Exchange
	dropped           int
	evidenceWrites    int
	evidenceWriteTime time.Duration
	retryWait         time.Duration
}

type contextKey struct{}

// With returns ctx carrying r.
func With(ctx context.Context, r *Request) context.Context {
	return context.WithValue(ctx, contextKey{}, r)
}

// From returns the recording ctx carries, or nil.
func From(ctx context.Context) *Request {
	r, _ := ctx.Value(contextKey{}).(*Request)
	return r
}

func (r *Request) addExchange(exchange Exchange) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.exchanges) >= maxExchanges {
		r.dropped++
		return
	}
	r.exchanges = append(r.exchanges, exchange)
}

// EvidenceWrite records one synchronous database write made while the
// customer waits (a credential attempt's evidence).
func (r *Request) EvidenceWrite(took time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evidenceWrites++
	r.evidenceWriteTime += took
}

// RetryWait records a backoff the executor slept before a same-route retry.
func (r *Request) RetryWait(took time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retryWait += took
}

// Summary is a recording reduced to what a log line carries.
type Summary struct {
	Exchanges []Exchange
	// Dropped counts exchanges past the bound.
	Dropped int
	// Oxy and Provider are the summed Done times of each peer's exchanges.
	Oxy, Provider time.Duration
	// ProviderExchanges counts upstream exchanges (attempts, key walks).
	ProviderExchanges int
	EvidenceWrites    int
	EvidenceWriteTime time.Duration
	RetryWait         time.Duration
}

// Summary returns what was recorded so far.
func (r *Request) Summary() Summary {
	if r == nil {
		return Summary{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	summary := Summary{
		Exchanges: append([]Exchange(nil), r.exchanges...), Dropped: r.dropped,
		EvidenceWrites: r.evidenceWrites, EvidenceWriteTime: r.evidenceWriteTime, RetryWait: r.retryWait,
	}
	for _, exchange := range r.exchanges {
		switch exchange.Peer {
		case PeerOxy:
			summary.Oxy += exchange.Done
		case PeerProvider:
			summary.Provider += exchange.Done
			summary.ProviderExchanges++
		}
	}
	return summary
}

// Attrs renders a summary as log attributes, in milliseconds. Every value is
// a duration, a count, a boolean or a status code.
func (s Summary) Attrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.Int64("oxyMs", s.Oxy.Milliseconds()),
		slog.Int64("upstreamMs", s.Provider.Milliseconds()),
		slog.Int("upstreamExchanges", s.ProviderExchanges),
		slog.Int("evidenceWrites", s.EvidenceWrites),
		slog.Int64("evidenceWriteMs", s.EvidenceWriteTime.Milliseconds()),
		slog.Int64("retryWaitMs", s.RetryWait.Milliseconds()),
	}
	if s.Dropped > 0 {
		attrs = append(attrs, slog.Int("exchangesDropped", s.Dropped))
	}
	exchanges := make([]exchangeLine, 0, len(s.Exchanges))
	for _, exchange := range s.Exchanges {
		exchanges = append(exchanges, exchangeLine{
			Peer: exchange.Peer, Reused: exchange.Reused,
			DNSMs: exchange.DNS.Milliseconds(), ConnectMs: exchange.Connect.Milliseconds(), TLSMs: exchange.TLS.Milliseconds(),
			TTFBMs: exchange.FirstByte.Milliseconds(), HeadersMs: exchange.Headers.Milliseconds(), DoneMs: exchange.Done.Milliseconds(),
			Status: exchange.Status,
		})
	}
	return append(attrs, slog.Any("exchanges", exchanges))
}

// exchangeLine is one exchange as a log line renders it.
type exchangeLine struct {
	Peer      Peer  `json:"peer"`
	Reused    bool  `json:"reused"`
	DNSMs     int64 `json:"dnsMs"`
	ConnectMs int64 `json:"connectMs"`
	TLSMs     int64 `json:"tlsMs"`
	TTFBMs    int64 `json:"ttfbMs"`
	HeadersMs int64 `json:"headersMs"`
	DoneMs    int64 `json:"doneMs"`
	Status    int   `json:"status"`
}

// Transport measures every exchange whose request context carries a
// recording, attributing it to peer. A request without one passes through
// untouched.
func Transport(base http.RoundTripper, peer Peer) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, peer: peer}
}

type transport struct {
	base http.RoundTripper
	peer Peer
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	recording := From(request.Context())
	if recording == nil {
		return t.base.RoundTrip(request)
	}
	measure := &measurement{recording: recording, exchange: Exchange{Peer: t.peer}, started: time.Now()}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), measure.trace()))
	response, err := t.base.RoundTrip(request)
	measure.mu.Lock()
	measure.exchange.Headers = time.Since(measure.started)
	measure.mu.Unlock()
	if err != nil {
		measure.finish()
		return nil, err
	}
	measure.mu.Lock()
	measure.exchange.Status = response.StatusCode
	measure.mu.Unlock()
	if response.StatusCode == http.StatusSwitchingProtocols {
		// A 101's body is the upgraded connection, written as well as read;
		// wrapping it would hide its Write. The exchange ends at the upgrade.
		measure.finish()
		return response, nil
	}
	response.Body = &measuredBody{ReadCloser: response.Body, measure: measure}
	return response, nil
}

// measurement is one exchange in flight. httptrace hooks can run on the
// transport's own goroutines, hence the lock.
type measurement struct {
	mu        sync.Mutex
	once      sync.Once
	recording *Request
	exchange  Exchange
	started   time.Time
	dnsStart  time.Time
	dialStart time.Time
	tlsStart  time.Time
}

func (m *measurement) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { m.mu.Lock(); m.dnsStart = time.Now(); m.mu.Unlock() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			m.mu.Lock()
			if !m.dnsStart.IsZero() {
				m.exchange.DNS += time.Since(m.dnsStart)
			}
			m.mu.Unlock()
		},
		ConnectStart: func(string, string) { m.mu.Lock(); m.dialStart = time.Now(); m.mu.Unlock() },
		ConnectDone: func(string, string, error) {
			m.mu.Lock()
			if !m.dialStart.IsZero() {
				m.exchange.Connect += time.Since(m.dialStart)
			}
			m.mu.Unlock()
		},
		TLSHandshakeStart: func() { m.mu.Lock(); m.tlsStart = time.Now(); m.mu.Unlock() },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			m.mu.Lock()
			if !m.tlsStart.IsZero() {
				m.exchange.TLS += time.Since(m.tlsStart)
			}
			m.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) { m.mu.Lock(); m.exchange.Reused = info.Reused; m.mu.Unlock() },
		GotFirstResponseByte: func() {
			m.mu.Lock()
			if m.exchange.FirstByte == 0 {
				m.exchange.FirstByte = time.Since(m.started)
			}
			m.mu.Unlock()
		},
	}
}

// finish records the exchange once, when its body closes or it fails.
func (m *measurement) finish() {
	m.once.Do(func() {
		m.mu.Lock()
		m.exchange.Done = time.Since(m.started)
		exchange := m.exchange
		m.mu.Unlock()
		m.recording.addExchange(exchange)
	})
}

type measuredBody struct {
	io.ReadCloser
	measure *measurement
}

func (b *measuredBody) Close() error {
	err := b.ReadCloser.Close()
	b.measure.finish()
	return err
}
