package requesttiming_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/requesttiming"
)

func get(t *testing.T, client *http.Client, ctx context.Context, url string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

// Each exchange of a measured request is recorded when its body closes: the
// first dials (not reused), the second rides the kept-alive connection. The
// control is the same client with no recording in the context: nothing is
// recorded, and the response is untouched.
func TestExchangesAreMeasuredOnlyForARecordedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	client := &http.Client{Transport: requesttiming.Transport(server.Client().Transport, requesttiming.PeerProvider)}

	control := &requesttiming.Request{}
	get(t, client, context.Background(), server.URL)
	if summary := control.Summary(); len(summary.Exchanges) != 0 {
		t.Fatalf("an unrecorded request was measured: %+v", summary)
	}

	recording := &requesttiming.Request{}
	ctx := requesttiming.With(context.Background(), recording)
	get(t, client, ctx, server.URL)
	get(t, client, ctx, server.URL)
	summary := recording.Summary()
	if len(summary.Exchanges) != 2 || summary.ProviderExchanges != 2 {
		t.Fatalf("exchanges = %+v, want 2", summary.Exchanges)
	}
	for index, exchange := range summary.Exchanges {
		if exchange.Peer != requesttiming.PeerProvider || exchange.Status != http.StatusAccepted ||
			exchange.FirstByte < 5*time.Millisecond || exchange.Done < exchange.FirstByte || exchange.Headers < exchange.FirstByte {
			t.Errorf("exchange %d = %+v", index, exchange)
		}
	}
	// The control request opened the connection, so both measured ones reuse it.
	if !summary.Exchanges[0].Reused || !summary.Exchanges[1].Reused || summary.Exchanges[1].Connect != 0 {
		t.Errorf("reuse = %+v", summary.Exchanges)
	}
	if summary.Provider != summary.Exchanges[0].Done+summary.Exchanges[1].Done || summary.Oxy != 0 {
		t.Errorf("per-peer totals = %+v", summary)
	}

	// A fresh client must dial: the measurement says so.
	fresh := &http.Client{Transport: requesttiming.Transport(&http.Transport{}, requesttiming.PeerOxy)}
	dialed := &requesttiming.Request{}
	get(t, fresh, requesttiming.With(context.Background(), dialed), server.URL)
	if exchanges := dialed.Summary().Exchanges; len(exchanges) != 1 || exchanges[0].Reused || exchanges[0].Peer != requesttiming.PeerOxy {
		t.Errorf("a fresh connection = %+v", exchanges)
	}
}

// A failed exchange is recorded once, with no status.
func TestAFailedExchangeIsRecorded(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	recording := &requesttiming.Request{}
	request, _ := http.NewRequestWithContext(requesttiming.With(context.Background(), recording), http.MethodGet, url, nil)
	client := &http.Client{Transport: requesttiming.Transport(&http.Transport{}, requesttiming.PeerProvider)}
	if response, err := client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("a closed server answered")
	}
	if exchanges := recording.Summary().Exchanges; len(exchanges) != 1 || exchanges[0].Status != 0 {
		t.Errorf("exchanges = %+v", exchanges)
	}
}

// The waits and writes the executor reports are summed; a nil recording
// accepts them as no-ops, so an unmeasured caller needs no branch.
func TestWaitsAndEvidenceWritesAreSummed(t *testing.T) {
	var none *requesttiming.Request
	none.RetryWait(time.Second)
	none.EvidenceWrite(time.Second)
	recording := &requesttiming.Request{}
	recording.RetryWait(time.Second)
	recording.RetryWait(2 * time.Second)
	recording.EvidenceWrite(3 * time.Millisecond)
	recording.EvidenceWrite(4 * time.Millisecond)
	summary := recording.Summary()
	if summary.RetryWait != 3*time.Second || summary.EvidenceWrites != 2 || summary.EvidenceWriteTime != 7*time.Millisecond {
		t.Errorf("summary = %+v", summary)
	}
	if requesttiming.From(context.Background()) != nil {
		t.Error("an unmeasured context carries a recording")
	}
}
