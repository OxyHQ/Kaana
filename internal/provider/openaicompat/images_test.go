package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

const imageUpstreamModel = "gpt-image-1"

func imageEnvelope(prompt string, parameters *contract.ImageParameters) *contract.Request {
	request := requestWith(nil)
	request.Modality = contract.ModalityImage
	request.Input = contract.Input{Format: contract.InputText, Text: &prompt}
	request.Stream = false
	request.Client.APIFormat = contract.APIFormatImagesGenerations
	request.Client.Endpoint = "/v1/images/generations"
	request.Image = parameters
	return request
}

func imageRoute(slug contract.ProviderSlug, upstreamModel string) provider.Route {
	return provider.Route{
		DeploymentID: "dep_image_wire", Provider: slug,
		ModelReference: "openai/gpt-image-1@2026-04-23", UpstreamModelID: upstreamModel,
	}
}

func imageUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	return upstream
}

// runImage drives Translate and Stream against a fake upstream.
func runImage(t *testing.T, upstream *httptest.Server, parameters *contract.ImageParameters) (provider.Outcome, error) {
	t.Helper()
	adapter := embeddingAdapter(t, "openai", upstream.URL)
	call, err := adapter.Translate(imageEnvelope("a lighthouse at dusk", parameters), imageRoute("openai", imageUpstreamModel))
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	return adapter.Stream(context.Background(), call, silentEmitter{}, nil)
}

// The table lists images for OpenAI and for no other built-in slug: a slug is
// added by a reviewed line, not by looking like OpenAI.
func TestTheCapabilityTableListsImagesForOpenAIOnly(t *testing.T) {
	var served []contract.ProviderSlug
	for _, slug := range compatibleBuiltInSlugs() {
		if slices.Contains(providerconfig.ExecutableAPIFormats(slug, providerconfig.ProtocolOpenAICompatible), contract.APIFormatImagesGenerations) {
			served = append(served, slug)
		}
	}
	if fmt.Sprint(served) != "[openai]" {
		t.Fatalf("images are served for %v, expected [openai]", served)
	}
}

func TestImagesOnTheUpstreamWire(t *testing.T) {
	var path string
	var body map[string]any
	var header http.Header
	upstream := imageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		path, header = r.URL.Path, r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"AAAA"},{"b64_json":"BBBB"}],"usage":{"input_tokens":12,"output_tokens":4160,"total_tokens":4172}}`)
	})
	format := contract.ImageResponseB64JSON
	two, size, quality, style := 2, "1024x1024", "high", "vivid"
	outcome, err := runImage(t, upstream, &contract.ImageParameters{N: &two, Size: &size, Quality: &quality, Style: &style, ResponseFormat: &format})
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if !strings.HasSuffix(path, "/images/generations") {
		t.Errorf("the request went to %q", path)
	}
	if header.Get("Content-Type") != "application/json" || !strings.HasPrefix(header.Get("Authorization"), "Bearer ") {
		t.Errorf("headers = %v", header)
	}
	want := map[string]any{"model": imageUpstreamModel, "prompt": "a lighthouse at dusk", "n": float64(2), "size": size, "quality": quality, "style": style, "response_format": "b64_json"}
	if fmt.Sprint(body) != fmt.Sprint(want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	if outcome.Image == nil || len(outcome.Image.Images) != 2 || outcome.Image.Images[0].B64JSON != "AAAA" {
		t.Fatalf("images = %+v", outcome.Image)
	}
	if outcome.UsageSource != contract.UsageProviderReported {
		t.Errorf("usage source = %s", outcome.UsageSource)
	}
	units := map[contract.UsageUnit]int{}
	for _, unit := range outcome.Units {
		units[unit.Unit] = unit.Quantity
	}
	if fmt.Sprint(units) != fmt.Sprint(map[contract.UsageUnit]int{contract.UnitImages: 2, contract.UnitInputTokens: 12, contract.UnitOutputTokens: 4160}) {
		t.Errorf("units = %v", units)
	}
}

// A control the caller did not name is not sent: nothing is defaulted.
func TestImagesSendOnlyTheNamedControls(t *testing.T) {
	var body map[string]any
	upstream := imageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = io.WriteString(w, `{"created":1,"data":[{"url":"https://images.example/1.png"}]}`)
	})
	outcome, err := runImage(t, upstream, nil)
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if len(body) != 2 {
		t.Errorf("body = %v; only model and prompt were named", body)
	}
	if outcome.Image == nil || outcome.Image.Images[0].URL != "https://images.example/1.png" {
		t.Fatalf("images = %+v", outcome.Image)
	}
	if len(outcome.Units) != 1 || outcome.Units[0].Unit != contract.UnitImages || outcome.Units[0].Quantity != 1 {
		t.Errorf("units = %+v; tokens are reported only when the provider reports them", outcome.Units)
	}
}

func TestImageProviderFailuresAreClassified(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		attributable bool
		code         contract.ErrorCode
	}{
		{"rate limit", http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_exceeded"}}`, true, contract.CodeRateLimited},
		{"server error", http.StatusInternalServerError, `{"error":{"message":"boom"}}`, true, contract.CodeProviderError},
		{"unavailable", http.StatusServiceUnavailable, `{"error":{"message":"busy"}}`, true, ""},
		{"bad request", http.StatusBadRequest, `{"error":{"message":"invalid size","type":"invalid_request_error"}}`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := imageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			outcome, err := runImage(t, upstream, nil)
			if err == nil {
				t.Fatal("a refused request succeeded")
			}
			if outcome.Image != nil {
				t.Error("a refused request produced images")
			}
			if got := provider.DeploymentAttributable(err); got != c.attributable {
				t.Errorf("attributable = %t, want %t (%v)", got, c.attributable, err)
			}
			var upstreamFailure provider.ErrUpstream
			if c.code != "" && (!errors.As(err, &upstreamFailure) || upstreamFailure.Code != c.code) {
				t.Errorf("failure = %v, want code %s", err, c.code)
			}
		})
	}
}

// An answer with no image, however it is shaped, is the deployment's failure:
// attributable and server-side, so the breaker, the retry and failover read it
// like any other, and no image or unit is reported (#266).
func TestAnImageAnswerWithNoImageIsAnAttributableFailure(t *testing.T) {
	for name, body := range map[string]string{
		"empty list":        `{"created":1,"data":[]}`,
		"no data member":    `{"created":1}`,
		"entries with none": `{"created":1,"data":[{},{"url":""},{"b64_json":""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			upstream := imageUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			outcome, err := runImage(t, upstream, nil)
			if err == nil || !provider.DeploymentAttributable(err) {
				t.Fatalf("err = %v; an attempt that produced no image is an attributable failure", err)
			}
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) || failure.Category != contract.UpstreamServerError {
				t.Errorf("failure = %v", err)
			}
			if outcome.Image != nil || len(outcome.Units) != 0 {
				t.Errorf("outcome = %+v", outcome)
			}
		})
	}
}

// Entries that carry nothing are dropped and the rest are served and metered
// as what they are.
func TestAPartialImageAnswerIsServedAndMeteredAsReturned(t *testing.T) {
	upstream := imageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"AAAA"},{}]}`)
	})
	three := 3
	outcome, err := runImage(t, upstream, &contract.ImageParameters{N: &three})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Image.Images) != 1 || outcome.Units[0].Quantity != 1 {
		t.Errorf("outcome = %+v", outcome)
	}
}

func TestMoreImagesThanRequestedIsAProviderFaultThatBillsNothing(t *testing.T) {
	upstream := imageUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"AAAA"},{"b64_json":"BBBB"}]}`)
	})
	outcome, err := runImage(t, upstream, nil)
	if err == nil || outcome.Image != nil || len(outcome.Units) != 0 {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
}

func TestAnUnreadableImageAnswerIsAProviderError(t *testing.T) {
	upstream := imageUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `<html>`) })
	if _, err := runImage(t, upstream, nil); err == nil {
		t.Fatal("garbage was served")
	}
}

// The caller withdrawing mid-exchange tears down the upstream call and ends
// the attempt as a cancellation, not as a provider failure.
func TestACancelledImageRequestEndsAsACancellation(t *testing.T) {
	arrived := make(chan struct{})
	upstream := imageUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// The body is read so the server notices the connection closing.
		_, _ = io.Copy(io.Discard, r.Body)
		close(arrived)
		<-r.Context().Done()
	})
	adapter := embeddingAdapter(t, "openai", upstream.URL)
	call, err := adapter.Translate(imageEnvelope("slow", nil), imageRoute("openai", imageUpstreamModel))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-arrived
		cancel()
	}()
	finished := make(chan error, 1)
	var outcome provider.Outcome
	go func() {
		var streamErr error
		outcome, streamErr = adapter.Stream(ctx, call, silentEmitter{}, nil)
		finished <- streamErr
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want a cancellation", err)
		}
		if outcome.Image != nil {
			t.Error("a cancelled request produced images")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream call outlived the cancellation")
	}
}

func TestImageTranslationRefusals(t *testing.T) {
	adapter := embeddingAdapter(t, "openai", "https://example.test/v1")
	if _, err := adapter.Translate(imageEnvelope("x", nil), imageRoute("openai", "gpt-6-astra")); !isUnsupported(err, contract.CodeUnsupportedModality) {
		t.Errorf("a text model asked for images: %v", err)
	}
	streamed := imageEnvelope("x", nil)
	streamed.Stream = true
	if _, err := adapter.Translate(streamed, imageRoute("openai", imageUpstreamModel)); !isUnsupported(err, contract.CodeInvalidRequest) {
		t.Errorf("a streamed image request: %v", err)
	}
	// A slug whose table does not list images never builds the call.
	other := embeddingAdapter(t, "groq", "https://example.test/v1")
	if _, err := other.Translate(imageEnvelope("x", nil), imageRoute("groq", imageUpstreamModel)); !isUnsupported(err, contract.CodeUnsupportedModality) {
		t.Errorf("an unlisted slug: %v", err)
	}
	// And an image model is not a chat model.
	chat := requestWith(nil)
	if _, err := adapter.Translate(chat, imageRoute("openai", imageUpstreamModel)); !isUnsupported(err, contract.CodeUnsupportedModality) {
		t.Errorf("an image model asked to chat: %v", err)
	}
}

func isUnsupported(err error, code contract.ErrorCode) bool {
	var refusal provider.ErrUnsupported
	return errors.As(err, &refusal) && refusal.Code == code
}
