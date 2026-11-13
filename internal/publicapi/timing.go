package publicapi

import (
	"context"
	"fmt"
	"github.com/OxyHQ/Kaana/internal/emf"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/requesttiming"
)

// requestTrace is one billable request's phases: what admit and execute
// measured, beside the exchanges internal/requesttiming recorded underneath
// them. It holds ids, an operation, an outcome code and durations — never the
// body, the credential, a header or a user id — so the line it becomes obeys
// docs/rules.md as it stands.
type requestTrace struct {
	recording *requesttiming.Request

	mu           sync.Mutex
	requestID    contract.RequestID
	grantID      string
	operation    contract.GrantOperation
	endpoint     string
	admitStarted time.Time
	grant        time.Duration
	execute      time.Duration
	outcome      string
}

type traceKey struct{}

func traceFrom(ctx context.Context) *requestTrace {
	trace, _ := ctx.Value(traceKey{}).(*requestTrace)
	return trace
}

// asked records that a request reached Oxy's authorization.
func (t *requestTrace) asked(requestID contract.RequestID, ask authorization, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requestID, t.operation, t.endpoint, t.admitStarted = requestID, ask.operation, ask.endpoint, at
}

// granted records the grant's round trip and, when Oxy granted it, its id.
func (t *requestTrace) granted(took time.Duration, grant *contract.Grant, failure *contract.PublicError) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.grant = took
	if grant != nil {
		t.grantID = grant.GrantID
	}
	if failure != nil {
		t.outcome = string(failure.Code)
	}
}

// executed records the executor's run and its outcome.
func (t *requestTrace) executed(took time.Duration, failure *contract.Error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.execute = took
	t.outcome = string(contract.OutcomeCompleted)
	if failure != nil {
		t.outcome = string(failure.Code)
	}
}

// timed measures one billable endpoint and writes its timing line once the
// response is written. The line is the operator's answer to "where did this
// request's time go": the grant from Oxy, each upstream exchange (connection
// reuse, time to first byte), the evidence written while the caller waited,
// retry backoff, and what is left, which is Kaana's own work. Settlement is
// not in it: it is written after the response (execute) and logged on its own.
func (s *Server) timed(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		trace := &requestTrace{recording: &requesttiming.Request{}}
		ctx := requesttiming.With(context.WithValue(r.Context(), traceKey{}, trace), trace.recording)
		handler(w, r.WithContext(ctx))
		s.logTiming(ctx, trace, started, time.Since(started))
	}
}

func (s *Server) logTiming(ctx context.Context, trace *requestTrace, started time.Time, total time.Duration) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.requestID == "" {
		// Refused before Oxy was asked (an unreadable body, a bad parameter):
		// there is no grant to measure, and the refusal is already the answer.
		return
	}
	summary := trace.recording.Summary()
	own := total - trace.grant - summary.Provider - summary.EvidenceWriteTime - summary.RetryWait
	attrs := []slog.Attr{
		slog.String("requestId", string(trace.requestID)),
		slog.String("grantId", trace.grantID),
		slog.String("operation", string(trace.operation)),
		slog.String("endpoint", trace.endpoint),
		slog.String("outcome", trace.outcome),
		slog.Int64("totalMs", total.Milliseconds()),
		slog.Int64("beforeGrantMs", trace.admitStarted.Sub(started).Milliseconds()),
		slog.Int64("grantMs", trace.grant.Milliseconds()),
		slog.Int64("executeMs", trace.execute.Milliseconds()),
		slog.Int64("kaanaMs", max(own, 0).Milliseconds()),
	}
	s.record(emf.PublicRequest, float64(total.Milliseconds()), emf.Labels{"Operation": string(trace.operation), "Outcome": trace.outcome})
	s.record(emf.GrantRoundTrip, float64(trace.grant.Milliseconds()), nil)
	if summary.ProviderExchanges > 0 {
		s.record(emf.UpstreamExchange, float64(summary.Provider.Milliseconds()), nil)
	}
	s.logger.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "public request timing", append(attrs, summary.Attrs()...)...)
}

// HeaderServerTiming is the W3C Server-Timing header.
const HeaderServerTiming = "Server-Timing"

// serverTiming is the caller's own answer to "where did my request's time go",
// in the standard header: the authorization round trip, the upstream exchanges
// and any retry backoff, each in milliseconds. It carries durations only: no
// provider, route or key is named, so it discloses nothing the response does
// not (docs/rules.md). A metric that was not measured is omitted, never
// reported as zero. It is stamped just before a non-streamed body is written,
// when these phases are complete; a stream has already sent its headers.
func (t *requestTrace) serverTiming() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	grant := t.grant
	t.mu.Unlock()
	summary := t.recording.Summary()
	var metrics []string
	add := func(name string, d time.Duration, measured bool) {
		if measured {
			metrics = append(metrics, fmt.Sprintf("%s;dur=%.1f", name, float64(d.Microseconds())/1000))
		}
	}
	add("grant", grant, grant > 0)
	add("upstream", summary.Provider, summary.ProviderExchanges > 0)
	add("retry", summary.RetryWait, summary.RetryWait > 0)
	return strings.Join(metrics, ", ")
}

// stampServerTiming sets Server-Timing from the request's trace, if any.
func stampServerTiming(ctx context.Context, header http.Header) {
	if value := traceFrom(ctx).serverTiming(); value != "" {
		header.Set(HeaderServerTiming, value)
	}
}
