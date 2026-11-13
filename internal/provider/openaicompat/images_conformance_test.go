package openaicompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/conformance"
)

// Every slug whose table lists images passes the images suite against a fake
// of OpenAI's real wire.
func TestImagesAdapterConforms(t *testing.T) {
	subject := conformance.ImagesSubject{
		Name: "openai-images", Provider: "openai",
		ModelReference: "openai/gpt-image-1@2026-04-23", UpstreamModelID: imageUpstreamModel,
		Images: 2, InputTokens: 12, OutputTokens: 4160,
		NewAdapter: func(t *testing.T, upstreamURL string) provider.Adapter {
			return embeddingAdapter(t, "openai", upstreamURL)
		},
		StartUpstream: startImagesFake,
	}
	conformance.RunImages(t, subject)
}

func startImagesFake(t *testing.T, scenario conformance.ImagesScenario) *conformance.ImagesUpstream {
	t.Helper()
	var mutex sync.Mutex
	requests, disconnected := 0, false
	arrived := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mutex.Lock()
		requests++
		mutex.Unlock()
		decoded := struct {
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			N              int    `json:"n"`
			Size           string `json:"size"`
			Quality        string `json:"quality"`
			Style          string `json:"style"`
			ResponseFormat string `json:"response_format"`
		}{}
		_ = json.Unmarshal(raw, &decoded)
		if !strings.HasSuffix(r.URL.Path, "/images/generations") || decoded.Model != imageUpstreamModel || decoded.Prompt != conformance.ImagesPrompt ||
			decoded.N != 2 || decoded.Size != "1024x1024" || decoded.Quality != "high" || decoded.Style != "vivid" || decoded.ResponseFormat != "b64_json" {
			t.Errorf("the upstream received %s %s", r.URL.Path, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		switch scenario {
		case conformance.ImagesAnswered:
			_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"AAAA"},{"b64_json":"BBBB"}],"usage":{"input_tokens":12,"output_tokens":4160,"total_tokens":4172}}`)
		case conformance.ImagesEmpty:
			_, _ = io.WriteString(w, `{"created":1,"data":[],"usage":{"input_tokens":12,"output_tokens":0,"total_tokens":12}}`)
		case conformance.ImagesRateLimited:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached","type":"rate_limit_exceeded"}}`)
		case conformance.ImagesServerError:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"The server had an error","type":"server_error"}}`)
		case conformance.ImagesRequestRefused:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid value for 'size'","type":"invalid_request_error","param":"size"}}`)
		case conformance.ImagesHeld:
			once.Do(func() { close(arrived) })
			<-r.Context().Done()
			mutex.Lock()
			disconnected = true
			mutex.Unlock()
		}
	}))
	t.Cleanup(server.Close)
	return &conformance.ImagesUpstream{
		URL:          server.URL,
		Requests:     func() int { mutex.Lock(); defer mutex.Unlock(); return requests },
		Disconnected: func() bool { mutex.Lock(); defer mutex.Unlock(); return disconnected },
		Arrived:      arrived,
	}
}
