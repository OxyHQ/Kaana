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

// answer scripts one exchange with a pinned upstream; n counts that
// upstream's exchanges from zero. It returns false when it wrote no vector.
type answer func(n int, w http.ResponseWriter, r *http.Request) bool

// hedgeUpstream answers each pinned upstream (the body's provider.only) as
// the test scripts it, and records which upstreams were asked, in order.
type hedgeUpstream struct {
	mu    sync.Mutex
	asked []string
	gone  map[string]int
}

func (u *hedgeUpstream) askedUpstreams() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.asked...)
}

func (u *hedgeUpstream) cancelled(upstream string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gone[upstream]
}

func serveHedge(t *testing.T, script map[string]answer) (*hedgeUpstream, *httptest.Server) {
	t.Helper()
	upstream := &hedgeUpstream{gone: map[string]int{}}
	vector, _ := json.Marshal(make([]float64, provider.EmbeddingDimension))
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
		n := 0
		for _, asked := range upstream.asked {
			if asked == name {
				n++
			}
		}
		upstream.asked = append(upstream.asked, name)
		upstream.mu.Unlock()
		if scripted, ok := script[name]; ok && !scripted(n, w, r) {
			if r.Context().Err() != nil {
				upstream.mu.Lock()
				upstream.gone[name]++
				upstream.mu.Unlock()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","model":"qwen/qwen3-embedding-8b","data":[{"object":"embedding","index":0,"embedding":%s}],"usage":{"prompt_tokens":4,"total_tokens":4}}`, vector)
	}))
	t.Cleanup(server.Close)
	return upstream, server
}

// stall holds an exchange until the client goes away.
func stall(_ int, _ http.ResponseWriter, r *http.Request) bool {
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
	return false
}

// stallFirst stalls an upstream's first exchange and answers the rest.
func stallFirst(n int, w http.ResponseWriter, r *http.Request) bool {
	if n == 0 {
		return stall(n, w, r)
	}
	return true
}

func status(code int) answer {
	return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"code":`+fmt.Sprint(code)+`,"message":"scripted"}}`)
		return false
	}
}

var hedgeRoute = provider.Route{
	DeploymentID: "dep_hedge", Provider: "openrouter",
	ModelReference: "qwen/qwen3-embedding-8b@2026-10-15", UpstreamModelID: "qwen/qwen3-embedding-8b",
}

func hedgedCall(t *testing.T, adapter *Adapter, after time.Duration) *provider.Call {
	t.Helper()
	call, err := adapter.Translate(embeddingEnvelope("embed me"), hedgeRoute)
	if err != nil {
		t.Fatal(err)
	}
	if call.Hedge == nil {
		t.Fatal("a pinned model with two upstreams was not hedged")
	}
	call.Hedge.After = after
	return call
}

func streamHedged(t *testing.T, adapter *Adapter, call *provider.Call) (provider.Outcome, *requesttiming.Request, time.Duration, error) {
	t.Helper()
	recording := &requesttiming.Request{}
	started := time.Now()
	outcome, err := adapter.Stream(requesttiming.With(context.Background(), recording), call, silentEmitter{}, nil)
	return outcome, recording, time.Since(started), err
}

// The pinned model's call is one body per upstream, each pinned to that
// upstream alone under the fixed zero-retention policy, raced over at most
// four exchanges; a short input is raced after the base bound and a 64K-token
// batch after its size allows.
func TestAPinnedEmbeddingIsRacedAcrossItsUpstreams(t *testing.T) {
	adapter := embeddingAdapter(t, "openrouter", "https://openrouter.ai/api/v1")
	call, err := adapter.Translate(embeddingEnvelope("embed me"), hedgeRoute)
	if err != nil {
		t.Fatal(err)
	}
	if call.Hedge == nil || len(call.Hedge.Bodies) != 2 || call.Hedge.Exchanges != 4 || call.Upstream != "deepinfra" {
		t.Fatalf("call = %q hedge = %+v", call.Upstream, call.Hedge)
	}
	for index, want := range []string{"deepinfra", "nebius"} {
		pinned := call.Hedge.Bodies[index]
		var body map[string]json.RawMessage
		if err := json.Unmarshal(pinned.Body, &body); err != nil || pinned.Upstream != want {
			t.Fatalf("body %d = %q %s", index, pinned.Upstream, pinned.Body)
		}
		assertPinnedOpenRouterProviderPolicy(t, body["provider"], []string{want})
	}
	if call.Hedge.After < embeddingHedgeBase || call.Hedge.After > embeddingHedgeBase+10*time.Millisecond {
		t.Errorf("a short input is raced after %s", call.Hedge.After)
	}
	batch := make([]string, 128)
	for index := range batch {
		batch[index] = strings.Repeat("abcd", 511)
	}
	if after := embeddingHedgeAfter(contract.Input{Format: contract.InputTextBatch, Texts: batch}); after < 13*time.Second || after > embeddingHedgeMax {
		t.Errorf("a 64K-token batch is raced after %s", after)
	}
	if after := embeddingHedgeAfter(contract.Input{Format: contract.InputTextBatch, Texts: append(batch, batch...)}); after != embeddingHedgeMax {
		t.Errorf("the bound is not capped: %s", after)
	}
}

// A slow first upstream is hedged: the other answers, serves the call, and
// the slow one is cancelled in flight and counted. The control is a first
// upstream that answers in time: nothing else is ever sent.
func TestASlowUpstreamIsHedgedAndAFastOneIsNot(t *testing.T) {
	upstream, server := serveHedge(t, map[string]answer{"deepinfra": stall})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	outcome, _, took, err := streamHedged(t, adapter, hedgedCall(t, adapter, 50*time.Millisecond))
	if err != nil || outcome.Embedding == nil {
		t.Fatalf("hedged call: %v", err)
	}
	if took > 2*time.Second || outcome.AbandonedExchanges != 1 {
		t.Errorf("took %s, abandoned %d; the hedge should have served", took, outcome.AbandonedExchanges)
	}
	deadline := time.Now().Add(2 * time.Second)
	for upstream.cancelled("deepinfra") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if upstream.cancelled("deepinfra") == 0 {
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

// When every exchange in flight has stalled, the race launches fresh ones,
// taking turns, rather than waiting on the stalled ones: here the third
// exchange, a fresh one to the first upstream, serves.
func TestAStalledRaceGetsFreshExchanges(t *testing.T) {
	upstream, server := serveHedge(t, map[string]answer{"deepinfra": stallFirst, "nebius": stall})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	outcome, _, took, err := streamHedged(t, adapter, hedgedCall(t, adapter, 50*time.Millisecond))
	if err != nil || outcome.Embedding == nil || took > 2*time.Second {
		t.Fatalf("err %v took %s", err, took)
	}
	if asked := upstream.askedUpstreams(); strings.Join(asked, " ") != "deepinfra nebius deepinfra" {
		t.Errorf("asked %v, want the upstreams taking turns", asked)
	}
	if outcome.AbandonedExchanges != 2 {
		t.Errorf("abandoned %d, want both stalled exchanges", outcome.AbandonedExchanges)
	}
}

// The race is capped: with every upstream stalled it launches four exchanges
// and no more.
func TestARaceLaunchesAtMostItsCap(t *testing.T) {
	upstream, server := serveHedge(t, map[string]answer{"deepinfra": stall, "nebius": stall})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if _, err := adapter.Stream(ctx, hedgedCall(t, adapter, 50*time.Millisecond), silentEmitter{}, nil); err == nil {
		t.Fatal("a race of stalled upstreams was served")
	}
	if asked := upstream.askedUpstreams(); len(asked) != 4 {
		t.Errorf("asked %v, want exactly four exchanges", asked)
	}
}

// The upstream that answered fastest lately goes first: after a race that
// the second upstream won while the first stalled, the next call starts with
// the second.
func TestTheFastestUpstreamLatelyGoesFirst(t *testing.T) {
	upstream, server := serveHedge(t, map[string]answer{"deepinfra": stallFirst})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	if _, _, _, err := streamHedged(t, adapter, hedgedCall(t, adapter, 50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := streamHedged(t, adapter, hedgedCall(t, adapter, time.Second)); err != nil {
		t.Fatal(err)
	}
	if asked := upstream.askedUpstreams(); strings.Join(asked, " ") != "deepinfra nebius nebius" {
		t.Errorf("asked %v, want the second call to start on nebius", asked)
	}
}

// A first upstream that fails transiently is hedged at once, without waiting
// out the bound; one that refuses the request is not, because the other would
// refuse it too.
func TestATransientFailureHedgesAtOnceAndARefusalDoesNot(t *testing.T) {
	upstream, server := serveHedge(t, map[string]answer{"deepinfra": status(http.StatusServiceUnavailable)})
	adapter := embeddingAdapter(t, "openrouter", server.URL)
	outcome, _, took, err := streamHedged(t, adapter, hedgedCall(t, adapter, time.Hour))
	if err != nil || outcome.Embedding == nil || took > 2*time.Second || outcome.AbandonedExchanges != 0 {
		t.Fatalf("err %v took %s abandoned %d", err, took, outcome.AbandonedExchanges)
	}
	if asked := upstream.askedUpstreams(); len(asked) != 2 || asked[1] != "nebius" {
		t.Errorf("asked %v", asked)
	}

	refused, refusedServer := serveHedge(t, map[string]answer{"deepinfra": status(http.StatusBadRequest)})
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
	_, server := serveHedge(t, map[string]answer{"deepinfra": stall})
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
	if _, abandoned := labels["deepinfra"]; labels["nebius"] != http.StatusOK || !abandoned || len(labels) != 2 {
		t.Errorf("exchanges by upstream = %v, want deepinfra (abandoned) and nebius (served)", labels)
	}
}
