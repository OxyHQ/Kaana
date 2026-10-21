package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/requesttiming"
)

// An embedding of a pinned OpenRouter model (providerconfig.
// OpenRouterUpstreamOrder) is sent to its first upstream alone and hedged
// with the rest after embeddingHedgeAfter.
//
// # Why a hedge, measured
//
// With the upstreams pinned (DeepInfra, then Nebius), 2026-10-21's timing
// lines put a four-token embedding's upstream time to first byte at 176-456 ms
// on almost every call, and at 8.1 s on one: a single slow exchange, no
// retry, on a connection already open. A tail like that is the upstream's
// queue on that request, and nothing about the request predicts it. Waiting
// longer does not shorten it; asking the other pinned upstream does. Between
// 09:20 and 09:47 the same lines show the other failure the hedge absorbs:
// OpenRouter answering large batches with a 504 after 60 s, which Kaana then
// retried on the same upstream, for up to 125 s, until the deployment's
// breaker opened and refused short queries too.
//
// # The bound
//
// embeddingHedgeBase is above the measured healthy time to first byte for a
// short input, with room, so a healthy call is almost never hedged, and with
// Oxy's grant (35-195 ms) and the hedge's own answer it keeps a short
// embedding well inside 1.5 s. A batch takes time in proportion to its input
// (5-12 s for Clarity's 64K-token indexing batches), so each estimated input
// token adds embeddingHedgePerToken: the hedge fires on an exchange that is
// far slower than its size explains, never on a large batch merely doing
// its work. The cap keeps a hedge inside a caller's 30 s timeout.
const (
	embeddingHedgeBase     = 600 * time.Millisecond
	embeddingHedgePerToken = 200 * time.Microsecond
	embeddingHedgeMax      = 20 * time.Second
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

// hedgedSender sends a call with a Hedge: Body first, the hedge after
// Hedge.After or at once when Body fails transiently, and serves the first
// complete 2xx answer. Refuse and TransportFailure are the adapter's own, so
// the key walk classifies a hedged answer exactly as a single one.
type hedgedSender struct {
	*Adapter

	mu sync.Mutex
	// abandoned counts exchanges cancelled in flight because the other one
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

// transientAnswer reports whether a failed exchange is one the other upstream
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

// Send implements provider.CredentialedSender.
func (s *hedgedSender) Send(ctx context.Context, call *provider.Call, key provider.Key) (*http.Response, error) {
	if call.Hedge == nil {
		return s.Adapter.Send(ctx, call, key)
	}
	results := make(chan hedgedResult, 2)
	var cancels [2]context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			if cancel != nil {
				cancel()
			}
		}
	}()
	start := func(index int, body []byte, upstream string) {
		exchangeContext, cancel := context.WithCancel(requesttiming.WithUpstream(ctx, upstream))
		cancels[index] = cancel
		variant := *call
		variant.Body, variant.Hedge = body, nil
		go func() { results <- s.exchange(exchangeContext, index, &variant, key) }()
	}
	start(0, call.Body, call.Upstream)
	inFlight, hedged := 1, false
	timer := time.NewTimer(call.Hedge.After)
	defer timer.Stop()
	hedge := func() {
		start(1, call.Hedge.Body, call.Hedge.Upstream)
		inFlight, hedged = inFlight+1, true
	}
	var failed hedgedResult
	for {
		var fire <-chan time.Time
		if !hedged {
			fire = timer.C
		}
		select {
		case <-fire:
			hedge()
		case result := <-results:
			inFlight--
			if success(result) {
				if inFlight > 0 {
					// The other exchange is still running: the deferred
					// cancel ends it, and it is counted as abandoned.
					s.mu.Lock()
					s.abandoned++
					s.mu.Unlock()
				}
				return result.response, nil
			}
			failed = result
			if !hedged && transientAnswer(result) && ctx.Err() == nil {
				hedge()
				continue
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

// exchange sends one hedged body and reads its whole answer before it
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
