package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/requesttiming"
)

// An embedding of a pinned OpenRouter model (providerconfig.
// OpenRouterUpstreamOrder) is raced across its upstreams: one exchange pinned
// to one upstream at once, another every embeddingHedgeAfter while none has
// answered, up to embeddingHedgeExchanges, the upstream answering fastest
// lately first.
//
// # Why, measured
//
// 2026-10-21's timing lines put a four-token embedding's upstream time to
// first byte at 176-456 ms on almost every call and at 8.1 s on one: a single
// slow exchange, no retry, on a connection already open. One hedge to the
// other upstream (#217) rescued most of those, and showed the rest. For
// minutes at a time DeepInfra answered nothing in under a second while Nebius
// answered in 0.4 s, so every call paid the full hedge bound before reaching
// the upstream that was answering. And now and then both pending exchanges
// stalled together for 6-7 s while a fresh exchange would have landed on an
// idle replica. So upstreams take turns, ordered by what this process
// measured, and a stalled race gets new exchanges rather than waiting on the
// stalled ones.
//
// # The bounds
//
// embeddingHedgeBase is above the measured healthy time to first byte for a
// short input, so a healthy call is rarely hedged, and with Oxy's grant
// (35-195 ms) four exchanges launch inside 1.5 s. A batch takes time in
// proportion to its input (2-12 s for Clarity's 64K-token indexing batches),
// so each estimated input token adds embeddingHedgePerToken. A batch is raced
// only when far slower than its size explains, never merely for doing its
// work. The cap keeps the second exchange inside a caller's 30 s timeout.
const (
	embeddingHedgeBase      = 450 * time.Millisecond
	embeddingHedgePerToken  = 200 * time.Microsecond
	embeddingHedgeMax       = 20 * time.Second
	embeddingHedgeExchanges = 4
)

// embeddingHedgeAfter is how long the first upstream may go without answering
// before the hedge is sent: the base, plus the per-token allowance for the
// input's estimated tokens (four bytes a token, one more per text, as the
// public API estimates a grant), capped.
func embeddingHedgeAfter(input contract.Input) time.Duration {
	texts := input.Texts
	if input.Format == contract.InputText {
		texts = []string{derefString(input.Text)}
	}
	tokens := 0
	for _, text := range texts {
		tokens += len(text)/4 + 1
	}
	return min(embeddingHedgeBase+time.Duration(tokens)*embeddingHedgePerToken, embeddingHedgeMax)
}

// pinnedEmbeddingBody encodes an embedding request served only by upstreams,
// in order, under the fixed zero-retention policy: the pin narrows the
// zero-retention set and never widens it.
func pinnedEmbeddingBody(embedding embeddingRequest, upstreams []string) ([]byte, error) {
	fallbacks := false
	pinned := embedding
	pinned.Provider = &openRouterProviderPolicy{
		ZDR: true, DataCollection: "deny", RequireParameters: true,
		Only: upstreams, Order: upstreams, AllowFallbacks: &fallbacks,
	}
	body, err := json.Marshal(pinned)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding embedding request: %w", err)
	}
	return body, nil
}

// hedgedSender races a call with a Hedge (provider.Hedge). Refuse and
// TransportFailure are the adapter's own, so the key walk classifies the
// answer it returns exactly as a single one.
type hedgedSender struct {
	*Adapter

	mu sync.Mutex
	// abandoned counts exchanges cancelled in flight because another one
	// answered first (provider.Outcome.AbandonedExchanges).
	abandoned int
}

type hedgedResult struct {
	index    int
	response *http.Response
	err      error
}

func (s *hedgedSender) abandonedExchanges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abandoned
}

// transientAnswer reports whether a failed exchange is one another exchange
// may survive: no answer at all, a throttle or an upstream fault. A refusal
// of the request or of the credential is identical on every upstream.
func transientAnswer(result hedgedResult) bool {
	if result.err != nil {
		return true
	}
	status := result.response.StatusCode
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func success(result hedgedResult) bool {
	return result.err == nil && result.response.StatusCode >= 200 && result.response.StatusCode < 300
}

type launched struct {
	upstream string
	at       time.Time
	pending  bool
}

// Send implements provider.CredentialedSender.
func (s *hedgedSender) Send(ctx context.Context, call *provider.Call, key provider.Key) (*http.Response, error) {
	if call.Hedge == nil || len(call.Hedge.Bodies) == 0 {
		return s.Adapter.Send(ctx, call, key)
	}
	model, after := call.Route.UpstreamModelID, call.Hedge.After
	plan := s.upstreams.order(model, call.Hedge.Bodies)
	limit := max(call.Hedge.Exchanges, 1)
	results := make(chan hedgedResult, limit)
	cancels := make([]context.CancelFunc, 0, limit)
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	exchanges := make([]launched, 0, limit)
	timer := time.NewTimer(after)
	defer timer.Stop()
	start := func() {
		pinned := plan[len(exchanges)%len(plan)]
		exchangeContext, cancel := context.WithCancel(requesttiming.WithUpstream(ctx, pinned.Upstream))
		cancels = append(cancels, cancel)
		index := len(exchanges)
		exchanges = append(exchanges, launched{upstream: pinned.Upstream, at: time.Now(), pending: true})
		variant := *call
		variant.Body, variant.Upstream, variant.Hedge = pinned.Body, pinned.Upstream, nil
		go func() { results <- s.exchange(exchangeContext, index, &variant, key) }()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(after)
	}
	start()
	inFlight, launching := 1, true
	var failed hedgedResult
	for {
		var fire <-chan time.Time
		if launching && len(exchanges) < limit {
			fire = timer.C
		}
		select {
		case <-fire:
			start()
			inFlight++
		case result := <-results:
			inFlight--
			exchange := &exchanges[result.index]
			exchange.pending = false
			took := time.Since(exchange.at)
			if success(result) {
				s.upstreams.observe(model, exchange.upstream, took, after)
				for index := range exchanges {
					if !exchanges[index].pending {
						continue
					}
					// Cancelled in flight by the deferred cancel. One that
					// had already waited longer than the winner took is
					// known to be at least that slow.
					s.mu.Lock()
					s.abandoned++
					s.mu.Unlock()
					if waited := time.Since(exchanges[index].at); waited > took {
						s.upstreams.observe(model, exchanges[index].upstream, waited, after)
					}
				}
				return result.response, nil
			}
			failed = result
			switch {
			case !transientAnswer(result):
				// Every upstream would refuse it: launch nothing more.
				launching = false
			case ctx.Err() != nil:
				launching = false
			default:
				s.upstreams.fail(model, exchange.upstream)
				if len(exchanges) < limit {
					start()
					inFlight++
					continue
				}
			}
			if inFlight > 0 {
				continue
			}
			// Every exchange failed: the walk classifies the last one.
			return failed.response, failed.err
		}
	}
}

// maxHedgedResponseBytes bounds one buffered answer; readEmbedding reads no
// more than this either.
const maxHedgedResponseBytes = 16 << 20

// exchange sends one pinned body and reads its whole answer before it
// competes: an exchange wins by delivering a complete body, not by sending
// headers and then stalling. The upstream body is closed here; what is raced
// is the buffered answer.
func (s *hedgedSender) exchange(ctx context.Context, index int, call *provider.Call, key provider.Key) hedgedResult {
	response, err := s.Adapter.Send(ctx, call, key)
	if err != nil {
		return hedgedResult{index: index, err: err}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHedgedResponseBytes+1))
	if err != nil {
		return hedgedResult{index: index, err: err}
	}
	buffered := *response
	buffered.Body = io.NopCloser(bytes.NewReader(body))
	return hedgedResult{index: index, response: &buffered}
}

// upstreamLatency is what this process measured of each pinned upstream,
// per model: a moving average of how long an exchange took relative to its
// hedge bound, so a batch and a four-token query are measured on one scale.
// It orders the upstreams of a race and never adds one: the reviewed set is
// providerconfig's, and an upstream nothing has measured keeps its reviewed
// place. It is per process, in memory, like the circuit breakers.
type upstreamLatency struct {
	mu     sync.Mutex
	scores map[string]float64
}

const (
	// upstreamLatencyWeight is how much one exchange moves the average.
	upstreamLatencyWeight = 0.3
	// upstreamFailurePenalty is a transient failure, in hedge bounds: as bad
	// as an exchange that outlasted a whole race.
	upstreamFailurePenalty = float64(embeddingHedgeExchanges)
)

func upstreamKey(model, upstream string) string { return model + "\x00" + upstream }

func (l *upstreamLatency) record(model, upstream string, sample float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.scores == nil {
		l.scores = make(map[string]float64)
	}
	key := upstreamKey(model, upstream)
	previous, seen := l.scores[key]
	if !seen {
		l.scores[key] = sample
		return
	}
	l.scores[key] = previous + upstreamLatencyWeight*(sample-previous)
}

func (l *upstreamLatency) observe(model, upstream string, took, after time.Duration) {
	if after <= 0 {
		return
	}
	l.record(model, upstream, float64(took)/float64(after))
}

func (l *upstreamLatency) fail(model, upstream string) {
	l.record(model, upstream, upstreamFailurePenalty)
}

// order returns the pinned bodies fastest first; an unmeasured upstream
// scores zero, and ties keep the reviewed order.
func (l *upstreamLatency) order(model string, bodies []provider.PinnedBody) []provider.PinnedBody {
	l.mu.Lock()
	scores := make([]float64, len(bodies))
	for index, body := range bodies {
		scores[index] = l.scores[upstreamKey(model, body.Upstream)]
	}
	l.mu.Unlock()
	indexes := make([]int, len(bodies))
	for index := range indexes {
		indexes[index] = index
	}
	sort.SliceStable(indexes, func(a, b int) bool { return scores[indexes[a]] < scores[indexes[b]] })
	ordered := make([]provider.PinnedBody, len(bodies))
	for position, index := range indexes {
		ordered[position] = bodies[index]
	}
	return ordered
}
