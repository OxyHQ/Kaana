package publicapi

import (
	"context"
	"log/slog"
	"net/http"
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
	s.logger.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "public request timing", append(attrs, summary.Attrs()...)...)
}
