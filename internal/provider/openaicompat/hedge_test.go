package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/requesttiming"
)

// hedgeUpstream answers each pinned upstream (the body's provider.only) as
// the test scripts it, and records which upstreams were asked.
type hedgeUpstream struct {
	mu    sync.Mutex
	asked []string
	gone  map[string]bool
}

func (u *hedgeUpstream) askedUpstreams() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.asked...)
}

func (u *hedgeUpstream) wasCancelled(upstream string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gone[upstream]
}

func serveHedge(t *testing.T, script map[string]func(w http.ResponseWriter, r *http.Request) bool) (*hedgeUpstream, *httptest.Server) {
	t.Helper()
	upstream := &hedgeUpstream{gone: map[string]bool{}}
	vector := make([]float64, provider.EmbeddingDimension)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Provider struct {
				Only []string `json:"only"`
			} `json:"provider"`
		}
		_ = json.Unmarshal(body, &request)
		name := strings.Join(request.Provider.Only, ",")
		upstream.mu.Lock()
		upstream.asked = append(upstream.asked, name)
		upstream.mu.Unlock()
		if answer, scripted := script[name]; scripted && !answer(w, r) {
			upstream.mu.Lock()
			upstream.gone[name] = true
			upstream.mu.Unlock()
			return
		}
		encoded, _ := json.Marshal(vector)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","model":"qwen/qwen3-embedding-8b","data":[{"object":"embedding","index":0,"embedding":%s}],"usage":{"prompt_tokens":4,"total_tokens":4}}`, encoded)
	}))
	t.Cleanup(server.Close)
	return upstream, server
}

// stall holds the exchange until the client goes away and reports that it did.
func stall(_ http.ResponseWriter, r *http.Request) bool {
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
	return false
}

func status(code int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"code":`+fmt.Sprint(code)+`,"message":"scripted"}}`)
		return false
	}
}

func hedgedCall(t *testing.T, adapter *Adapter, after time.Duration) *provider.Call {
	t.Helper()
	call, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{
		DeploymentID: "dep_hedge", Provider: "openrouter",
		ModelReference: "qwen/qwen3-embedding-8b@2026-10-15", UpstreamModelID: "qwen/qwen3-embedding-8b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Hedge == nil {
		t.Fatal("a pinned model with two upstreams was not hedged")
	}
	call.Hedge.After = after
	return call
}

// The pinned model's call goes to its first upstream alone, with the rest as
// the hedge, both under the fixed zero-retention policy; a short input is
// hedged after the base bound and a 64K-token batch after its size allows.
func TestAPinnedEmbeddingIsHedgedWithItsOtherUpstreams(t *testing.T) {
	adapter := embeddingAdapter(t, "openrouter", "https://openrouter.ai/api/v1")
	call, err := adapter.Translate(embeddingEnvelope("embed me"), provider.Route{
		DeploymentID: "dep_hedge", Provider: "openrouter",
		ModelReference: "qwen/qwen3-embedding-8b@2026-10-15", UpstreamModelID: "qwen/qwen3-embedding-8b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Upstream != "deepinfra" || call.Hedge == nil || call.Hedge.Upstream != "nebius" {
		t.Fatalf("call = %q hedge = %+v", call.Upstream, call.Hedge)
	}
	var hedge map[string]json.RawMessage
	if err := json.Unmarshal(call.Hedge.Body, &hedge); err != nil {
		t.Fatal(err)
	}
	assertPinnedOpenRouterProviderPolicy(t, hedge["provider"], []string{"nebius"})
	if call.Hedge.After < embeddingHedgeBase || call.Hedge.After > embeddingHedgeBase+10*time.Millisecond {
		t.Errorf("a short input is hedged after %s", call.Hedge.After)
	}
	batch := make([]string, 128)
	for index := range batch {
		batch[index] = strings.Repeat("abcd", 511)
	}
	if after := embeddingHedgeAfter(contract.Input{Format: contract.InputTextBatch, Texts: batch}); after < 13*time.Second || after > embeddingHedgeMax {
		t.Errorf("a 64K-token batch is hedged after %s", after)
	}
	if after := embeddingHedgeAfter(contract.Input{Format: contract.InputTextBatch, Texts: append(batch, batch...)}); after != embeddingHedgeMax {
		t.Errorf("the bound is not capped: %s", after)
	}
}

func streamHedged(t *testing.T, adapter *Adapter, call *provider.Call) (provider.Outcome, *requesttiming.Request, time.Duration, error) {
	t.Helper()
	recording := &requesttiming.Request{}
	started := time.Now()
	outcome, err := adapter.Stream(requesttiming.With(context.Background(), recording), call, silentEmitter{}, nil)
	return outcome, recording, time.Since(started), err
}

// A slow first upstream is hedged: the other answers, serves the call, and
// the slow one is cancelled in flight and counted. The control is a first
// upstream that answers in time: no hedge is ever sent.
func TestASlowUpstreamIsHedgedAndAFastOneIsNot(t *testing.T) {
	upstream, server := serveHedge(t, map[string]func(http.ResponseWriter, *http.Request) bool{"deepinfra": stall})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	outcome, _, took, err := streamHedged(t, adapter, hedgedCall(t, adapter, 50*time.Millisecond))
	if err != nil || outcome.Embedding == nil {
		t.Fatalf("hedged call: %v", err)
	}
	if took > 2*time.Second || outcome.AbandonedExchanges != 1 {
		t.Errorf("took %s, abandoned %d; the hedge should have served", took, outcome.AbandonedExchanges)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !upstream.wasCancelled("deepinfra") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !upstream.wasCancelled("deepinfra") {
		t.Error("the abandoned exchange was never cancelled upstream")
	}

	control, controlServer := serveHedge(t, nil)
	controlAdapter := embeddingAdapter(t, "openrouter", controlServer.URL)
	outcome, _, _, err = streamHedged(t, controlAdapter, hedgedCall(t, controlAdapter, time.Second))
	if err != nil || outcome.AbandonedExchanges != 0 {
		t.Fatalf("control: %v abandoned %d", err, outcome.AbandonedExchanges)
	}
	if asked := control.askedUpstreams(); len(asked) != 1 || asked[0] != "deepinfra" {
		t.Errorf("control asked %v, want the first upstream alone", asked)
	}
}

// A first upstream that fails transiently is hedged at once, without waiting
// out the bound; one that refuses the request is not, because the other would
// refuse it too.
func TestATransientFailureHedgesAtOnceAndARefusalDoesNot(t *testing.T) {
	upstream, server := serveHedge(t, map[string]func(http.ResponseWriter, *http.Request) bool{"deepinfra": status(http.StatusServiceUnavailable)})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	outcome, _, took, err := streamHedged(t, adapter, hedgedCall(t, adapter, time.Hour))
	if err != nil || outcome.Embedding == nil || took > 2*time.Second || outcome.AbandonedExchanges != 0 {
		t.Fatalf("err %v took %s abandoned %d", err, took, outcome.AbandonedExchanges)
	}
	if asked := upstream.askedUpstreams(); len(asked) != 2 || asked[1] != "nebius" {
		t.Errorf("asked %v", asked)
	}

	refused, refusedServer := serveHedge(t, map[string]func(http.ResponseWriter, *http.Request) bool{"deepinfra": status(http.StatusBadRequest)})
	refusedAdapter := embeddingAdapter(t, "openrouter", refusedServer.URL)
	if _, _, _, err := streamHedged(t, refusedAdapter, hedgedCall(t, refusedAdapter, time.Hour)); err == nil {
		t.Fatal("a refused request was served")
	}
	if asked := refused.askedUpstreams(); len(asked) != 1 {
		t.Errorf("a refusal was hedged: asked %v", asked)
	}
}

// Each exchange is labelled with the upstream it was pinned to, so the timing
// line says which upstream was slow and which served.
func TestHedgedExchangesAreLabelledWithTheirUpstream(t *testing.T) {
	_, server := serveHedge(t, map[string]func(http.ResponseWriter, *http.Request) bool{"deepinfra": stall})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	adapter.client = &http.Client{Transport: requesttiming.Transport(adapter.client.Transport, requesttiming.PeerProvider)}
	_, recording, _, err := streamHedged(t, adapter, hedgedCall(t, adapter, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(recording.Summary().Exchanges) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	labels := map[string]int{}
	for _, exchange := range recording.Summary().Exchanges {
		labels[exchange.Upstream] = exchange.Status
	}
	if labels["nebius"] != http.StatusOK || len(labels) != 2 {
		t.Errorf("exchanges by upstream = %v, want deepinfra (abandoned) and nebius (served)", labels)
	}
	if _, ok := labels["deepinfra"]; !ok {
		t.Errorf("the abandoned exchange is missing: %v", labels)
	}
}
