package openaicompat

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

type spokenCapture struct {
	started    bool
	transcript strings.Builder
	audio      []byte
	units      []contract.UsageQuantity
}

func (s *spokenCapture) Start(contract.ModelReference, time.Time) error { s.started = true; return nil }
func (s *spokenCapture) Delta(_ int, channel contract.DeltaChannel, text string) error {
	if channel != contract.ChannelOutputAudioTranscript {
		return errors.New("a spoken answer wrote text")
	}
	s.transcript.WriteString(text)
	return nil
}
func (s *spokenCapture) ToolCall(provider.ToolCallDelta) error { return errors.New("unexpected tool") }
func (s *spokenCapture) Usage(units []contract.UsageQuantity, _ contract.UsageSource) error {
	s.units = units
	return nil
}
func (s *spokenCapture) Audio(_ int, _ string, data []byte) error {
	if !s.started || len(data) > provider.MaxAudioChunkBytes {
		return errors.New("invalid audio chunk")
	}
	s.audio = append(s.audio, data...)
	return nil
}

func spokenRequest(modality contract.Modality) *contract.Request {
	request := requestWith([]contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{textPartOf("Say hello")}}})
	request.Client.APIFormat = contract.APIFormatChatCompletions
	request.Modality = modality
	request.AudioOutput = &contract.AudioOutputParameters{Voice: "alloy", Format: contract.AudioOutputPCM}
	return request
}

func openRouterRoute(model string) provider.Route {
	return provider.Route{DeploymentID: "dep_or", Provider: "openrouter", ModelReference: contract.ModelReference(model + "@observed-2026-09-30"), UpstreamModelID: model}
}

// TestSpokenOutputOnlyWhereTheDeploymentSpeaks: this adapter answers aloud
// only for OpenRouter's rows of OpenAI's audio chat models. A text model on
// OpenRouter, and an audio model id on any other OpenAI-compatible slug, is
// refused naming `audioOutput` — also when the rest of the envelope looks like
// a text chat, so the refusal does not rest on the modality check that follows
// it. The OpenRouter audio row translating is the positive control.
func TestSpokenOutputOnlyWhereTheDeploymentSpeaks(t *testing.T) {
	gateway, err := New(Config{Provider: "openrouter", BaseURL: providerconfig.Known["openrouter"].BaseURL, Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatal(err)
	}
	direct := testAdapter(t)
	for _, c := range []struct {
		adapter *Adapter
		route   provider.Route
	}{
		{gateway, openRouterRoute("openai/gpt-6-astra")},
		{gateway, openRouterRoute("anthropic/claude-sonnet-5.5")},
		{direct, provider.Route{DeploymentID: "dep_openai", Provider: "openai", ModelReference: "openai/gpt-audio-1.5@observed-2026-09-30", UpstreamModelID: "gpt-audio-1.5"}},
	} {
		for _, modality := range []contract.Modality{contract.ModalityAudio, contract.ModalityText} {
			_, err := c.adapter.Translate(spokenRequest(modality), c.route)
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Param != "audioOutput" || unsupported.Code != contract.CodeUnsupportedModality {
				t.Errorf("%s/%s with %s modality: err = %v; spoken output must be refused by name", c.adapter.Provider(), c.route.UpstreamModelID, modality, err)
			}
		}
		if outputs := c.adapter.ChatOutputs(c.route); outputs.Spoken || !outputs.Text {
			t.Errorf("%s/%s declares %+v", c.adapter.Provider(), c.route.UpstreamModelID, outputs)
		}
	}
	for _, model := range []string{"openai/gpt-audio", "openai/gpt-audio-mini"} {
		call, err := gateway.Translate(spokenRequest(contract.ModalityAudio), openRouterRoute(model))
		if err != nil || call.AudioMediaType != "audio/pcm" || !call.Stream {
			t.Fatalf("%s: a spoken request was not translated for the spoken wire: %+v, %v", model, call, err)
		}
		if outputs := gateway.ChatOutputs(openRouterRoute(model)); !outputs.Spoken || !outputs.Text {
			t.Errorf("%s declares %+v; it both speaks and writes", model, outputs)
		}
		// The same model still takes an ordinary text chat on the text path.
		text := spokenRequest(contract.ModalityText)
		text.AudioOutput = nil
		call, err = gateway.Translate(text, openRouterRoute(model))
		if err != nil || call.AudioMediaType != "" || strings.Contains(string(call.Body), `"modalities"`) {
			t.Fatalf("%s: a text chat was not translated for the text path: %v", model, err)
		}
	}
}

// TestOpenRouterSpokenStreamFailureKeepsWhatWasMeasured is a failure after
// the 200: OpenRouter reports a mid-stream error as an `error` object on a
// chunk (https://openrouter.ai/docs/api-reference/errors). The audio already
// delivered stays delivered, the failure is classified, and a stream that
// reported usage before failing keeps those units.
func TestOpenRouterSpokenStreamFailureKeepsWhatWasMeasured(t *testing.T) {
	audio := make([]byte, 60000)
	for index := range audio {
		audio[index] = byte(index)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"audio":{"data":"`+base64.StdEncoding.EncodeToString(audio)+`","transcript":"Hola"}}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":40,"prompt_tokens_details":{"audio_tokens":6},"completion_tokens_details":{"audio_tokens":30}}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"error":{"code":502,"message":"Provider disconnected; key `+fakeAPIKey+`"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`+"\n\n")
	}))
	defer upstream.Close()
	adapter, err := New(Config{Provider: "openrouter", BaseURL: providerconfig.Known["openrouter"].BaseURL,
		Declarations: provider.DeclareKeys([]string{fakeAPIKey}), HTTPClient: identityBoundFakeClient(t, upstream.URL)})
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Translate(spokenRequest(contract.ModalityAudio), openRouterRoute("openai/gpt-audio-mini"))
	if err != nil {
		t.Fatal(err)
	}
	out := &spokenCapture{}
	outcome, err := adapter.Stream(context.Background(), call, out, nil)
	var upstreamErr provider.ErrUpstream
	if !errors.As(err, &upstreamErr) || upstreamErr.Code != contract.CodeProviderError {
		t.Fatalf("err = %v; a failure after the 200 must be reported, not a completed answer", err)
	}
	if upstreamErr.Passthrough == nil || upstreamErr.Passthrough.Message == nil || strings.Contains(*upstreamErr.Passthrough.Message, fakeAPIKey) {
		t.Errorf("the diagnostic was lost or carries the key: %+v", upstreamErr.Passthrough)
	}
	if len(out.audio) != len(audio) || out.transcript.String() != "Hola" {
		t.Errorf("delivered %d bytes, transcript %q", len(out.audio), out.transcript.String())
	}
	if outcome.UsageSource != contract.UsageProviderReported || len(outcome.Units) == 0 || outcome.KeyID == "" {
		t.Errorf("outcome = %+v; the units measured before the failure were lost", outcome)
	}
}
