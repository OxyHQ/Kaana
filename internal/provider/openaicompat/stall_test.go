package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

func stallingUpstream(t *testing.T, frames bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for index := 0; index < 20; index++ {
			if frames {
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
			} else {
				// What OpenRouter sends while it waits on a model: a comment,
				// never a frame.
				_, _ = fmt.Fprint(w, ": OPENROUTER PROCESSING\n\n")
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func streamThrough(t *testing.T, server *httptest.Server) (time.Duration, error) {
	t.Helper()
	adapter, err := New(Config{Provider: "openai", BaseURL: server.URL, StreamIdleTimeout: 400 * time.Millisecond,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}
	call := &provider.Call{
		Route:  provider.Route{Provider: "openai", ModelReference: "openai/test-model@2026-05-01", UpstreamModelID: "test-model"},
		Method: http.MethodPost, URL: server.URL + "/chat/completions", Body: []byte(`{}`), Stream: true,
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}
	started := time.Now()
	_, streamErr := adapter.Stream(context.Background(), call, silentEmitter{}, nil)
	return time.Since(started), streamErr
}

func TestAStreamThatOnlyKeepsAliveTimesOut(t *testing.T) {
	elapsed, streamErr := streamThrough(t, stallingUpstream(t, false))
	var upstream provider.ErrUpstream
	if !errors.As(streamErr, &upstream) || upstream.Code != contract.CodeProviderTimeout || upstream.Category != contract.UpstreamTimeout {
		t.Fatalf("a stream of keep-alives was reported as %v", streamErr)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("the stall was bounded only after %s", elapsed)
	}
}

func TestAStreamThatKeepsSendingFramesIsNeverCutShort(t *testing.T) {
	// Control: 2s of frames 100ms apart under a 400ms idle bound completes, so
	// the bound measures silence, not the stream's length.
	elapsed, streamErr := streamThrough(t, stallingUpstream(t, true))
	if streamErr != nil {
		t.Fatalf("a steadily producing stream failed after %s: %v", elapsed, streamErr)
	}
}
