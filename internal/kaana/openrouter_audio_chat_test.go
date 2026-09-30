package kaana_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// speaking is a scripted adapter that declares, as the audio adapter does,
// that its chat_completions path answers aloud.
type speaking struct{ *scriptedAdapter }

func (speaking) ChatOutputs(provider.Route) providerconfig.ChatOutput {
	return providerconfig.ChatOutput{Spoken: true}
}

const openRouterAudioDeployments = `{"deploymentId":"dep_or_audio_mini","provider":"openrouter","modelReference":"openai/gpt-audio-mini@observed-2026-08-23","upstreamModelId":"openai/gpt-audio-mini","regions":[],"current":true},` +
	`{"deploymentId":"dep_or_text","provider":"openrouter","modelReference":"openai/gpt-6-astra@observed-2026-09-30","upstreamModelId":"openai/gpt-6-astra","regions":[],"current":true}`

func openRouterRoute(deployment contract.DeploymentID, reference contract.ModelReference) []contract.AuthorizedRoute {
	return []contract.AuthorizedRoute{{Substitution: contract.SubstitutionSameModel, DeploymentID: deployment, ModelReference: reference, Provider: "openrouter", Regions: []contract.Region{}}}
}

func openRouterSpokenRequest(deployment contract.DeploymentID, reference contract.ModelReference, stream bool, format contract.AudioOutputFormat) *contract.Request {
	request := spokenChatRequest()
	request.Target.ModelReference = &reference
	request.Stream = stream
	request.AudioOutput = &contract.AudioOutputParameters{Voice: "alloy", Format: format}
	request.AuthorizedRoutes = openRouterRoute(deployment, reference)
	return request
}

// openRouterUpstream is a fake OpenRouter speaking its documented spoken-output
// wire: an SSE stream with OpenRouter's keep-alive comment, `delta.audio`
// frames, a usage chunk (usage is always included, with OpenRouter's `cost`
// beside the OpenAI-shaped token details) and `[DONE]`.
func openRouterUpstream(t *testing.T, audio []byte, check func(body map[string]any)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer openrouter-audio-executor-test-secret" {
			t.Errorf("wrong OpenRouter transport: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("the body is not JSON: %v", err)
		}
		check(body)
		frame := func(value any) string {
			encoded, _ := json.Marshal(value)
			return "data: " + string(encoded) + "\n\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": OPENROUTER PROCESSING\n\n")
		half := len(audio) / 2
		for index, part := range [][]byte{audio[:half], audio[half:]} {
			transcript := map[int]string{0: "Hola", 1: " mundo"}[index]
			_, _ = io.WriteString(w, frame(map[string]any{"id": "gen-synthetic", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"role": "assistant", "audio": map[string]any{"data": base64.StdEncoding.EncodeToString(part), "transcript": transcript}}}}}))
		}
		_, _ = io.WriteString(w, frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		_, _ = io.WriteString(w, `data: {"id":"gen-synthetic","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":300,"total_tokens":420,`+
			`"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0,"audio_tokens":80,"video_tokens":0},`+
			`"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":260,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0},`+
			`"cost":0.0007,"is_byok":false,"cost_details":{"upstream_inference_cost":null}}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func openRouterAdapter(t *testing.T, target string) provider.Adapter {
	t.Helper()
	adapter, err := openaicompat.New(openaicompat.Config{
		Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
		Declarations: []provider.KeyDeclaration{{KeyID: "openrouter-test", Secret: "openrouter-audio-executor-test-secret"}},
		HTTPClient:   &http.Client{Transport: redirectTransport{target: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// TestSpokenChatExecutesThroughOpenRouterAndSettlesAudioTokens runs a spoken
// answer from OpenRouter's `openai/gpt-audio-mini` end to end: the shared
// spoken wire with OpenRouter's mandatory policy, audio and transcript on their
// own events, and every audio unit settled and priced at its own rate. The
// customer asked for a WHOLE wav answer; OpenRouter only streams spoken output,
// so the upstream is streamed and the same events are delivered.
func TestSpokenChatExecutesThroughOpenRouterAndSettlesAudioTokens(t *testing.T) {
	for _, c := range []struct {
		name      string
		stream    bool
		format    contract.AudioOutputFormat
		wire      string
		mediaType string
	}{
		{"streamed pcm", true, contract.AudioOutputPCM, "pcm16", "audio/pcm"},
		{"whole wav", false, "wav", "wav", "audio/wav"},
	} {
		t.Run(c.name, func(t *testing.T) {
			audio := bytes.Repeat([]byte{9, 8, 7, 6}, 15000) // 60000 bytes: two frames, three contract chunks
			server, calls := openRouterUpstream(t, audio, func(body map[string]any) {
				want := map[string]any{
					"model": "openai/gpt-audio-mini", "modalities": []any{"text", "audio"},
					"audio": map[string]any{"voice": "alloy", "format": c.wire}, "stream": true,
					"stream_options": map[string]any{"include_usage": true},
					"provider":       map[string]any{"zdr": true, "data_collection": "deny", "require_parameters": true},
				}
				for key, value := range want {
					if !reflect.DeepEqual(body[key], value) {
						t.Errorf("%s = %v, want %v", key, body[key], value)
					}
				}
				if len(body) != len(want)+1 { // + messages
					t.Errorf("body keys = %v; an unset control must not be invented", body)
				}
			})
			cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"rc_or_audio","source":"operator","sourceVersion":"test-fixture","observedAt":"2026-09-30T00:00:00Z","effectiveAt":"2026-09-30T00:00:00Z","rateCards":[{"deploymentId":"dep_or_audio_mini","currency":"XTS","rates":[
				{"unit":"requests","amountPerUnit":0},
				{"unit":"input_tokens","amountPerUnit":1},
				{"unit":"audio_input_tokens","amountPerUnit":3},
				{"unit":"output_tokens","amountPerUnit":5},
				{"unit":"audio_output_tokens","amountPerUnit":7}
			]}]}`))
			if err != nil {
				t.Fatal(err)
			}
			request := openRouterSpokenRequest("dep_or_audio_mini", "openai/gpt-audio-mini@observed-2026-08-23", c.stream, c.format)
			if err := request.Validate(); err != nil {
				t.Fatalf("the fixture is not a valid envelope: %v", err)
			}
			events, result := (harness{deployments: openRouterAudioDeployments, adapters: []provider.Adapter{openRouterAdapter(t, server.URL)}, costs: cards}).run(t, request)
			if result.Failure != nil || result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted || calls.Load() != 1 {
				t.Fatalf("result: %+v (%d upstream calls)", result, calls.Load())
			}
			want := []contract.UsageQuantity{
				{Unit: contract.UnitRequests, Quantity: 1},
				{Unit: contract.UnitInputTokens, Quantity: 40},
				{Unit: contract.UnitAudioInputTokens, Quantity: 80},
				{Unit: contract.UnitOutputTokens, Quantity: 40},
				{Unit: contract.UnitAudioOutputTokens, Quantity: 260},
			}
			if !reflect.DeepEqual(result.Report.Units, want) || result.Report.UsageSource != contract.UsageProviderReported {
				t.Fatalf("settled %+v (%s), want %+v", result.Report.Units, result.Report.UsageSource, want)
			}
			// 40*1 + 80*3 + 40*5 + 260*7 = 2300.
			if len(result.UpstreamCost.Totals) != 1 || result.UpstreamCost.Totals[0].Amount != 2300 {
				t.Errorf("upstream cost = %+v", result.UpstreamCost)
			}
			var transcript strings.Builder
			var decoded []byte
			for _, event := range events {
				switch e := event.(type) {
				case *contract.StreamDeltaEvent:
					if e.Channel != contract.ChannelOutputAudioTranscript {
						t.Errorf("a spoken answer wrote on %s", e.Channel)
					}
					transcript.WriteString(e.Text)
				case *contract.StreamAudioEvent:
					if string(e.MediaType) != c.mediaType {
						t.Errorf("media type %q, want %q", e.MediaType, c.mediaType)
					}
					part, _ := base64.StdEncoding.DecodeString(e.Data)
					decoded = append(decoded, part...)
				}
			}
			if transcript.String() != "Hola mundo" || !bytes.Equal(decoded, audio) {
				t.Errorf("transcript %q; audio intact %t", transcript.String(), bytes.Equal(decoded, audio))
			}
		})
	}
}

// TestTheExecutorDecidesSpeechPerDeployment is the gate: a spoken request to
// an OpenRouter TEXT model is refused before anything is sent, a text chat to
// the speak-only OpenAI audio deployment likewise, and a text chat to the
// OpenRouter audio model still runs on the text path. The spoken OpenRouter run
// above is the positive control for the first refusal.
func TestTheExecutorDecidesSpeechPerDeployment(t *testing.T) {
	t.Run("spoken request to a text model", func(t *testing.T) {
		server, calls := openRouterUpstream(t, []byte{1, 2}, func(map[string]any) {})
		request := openRouterSpokenRequest("dep_or_text", "openai/gpt-6-astra@observed-2026-09-30", true, contract.AudioOutputPCM)
		_, result := (harness{deployments: openRouterAudioDeployments, adapters: []provider.Adapter{openRouterAdapter(t, server.URL)}}).run(t, request)
		if result.Failure == nil || result.Failure.Code != contract.CodeUnsupportedModality || result.Failure.Param == nil || *result.Failure.Param != "audioOutput" {
			t.Fatalf("result: %+v", result)
		}
		if calls.Load() != 0 {
			t.Error("a spoken request reached OpenRouter for a model that cannot answer aloud")
		}
	})
	t.Run("text chat to the speak-only audio deployment", func(t *testing.T) {
		adapter := speaking{&scriptedAdapter{slug: "openai-audio"}}
		request := spokenChatRequest()
		request.AudioOutput, request.Modality = nil, contract.ModalityText
		_, result := (harness{deployments: audioChatDeployment, adapters: []provider.Adapter{adapter}}).run(t, request)
		if result.Failure == nil || result.Failure.Code != contract.CodeInvalidRequest || adapter.calls != 0 {
			t.Fatalf("result: %+v, %d calls", result, adapter.calls)
		}
	})
	t.Run("text chat to the OpenRouter audio model", func(t *testing.T) {
		text := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, spoken := body["modalities"]; spoken || body["audio"] != nil {
				t.Errorf("a text chat asked for audio: %v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hola"}}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer text.Close()
		reference := contract.ModelReference("openai/gpt-audio-mini@observed-2026-08-23")
		request := baseRequest()
		request.Target.ModelReference = &reference
		request.Client.APIFormat, request.Client.Endpoint = contract.APIFormatChatCompletions, "/v1/chat/completions"
		request.AuthorizedRoutes = openRouterRoute("dep_or_audio_mini", reference)
		_, result := (harness{deployments: openRouterAudioDeployments, adapters: []provider.Adapter{openRouterAdapter(t, text.URL)}}).run(t, request)
		if result.Failure != nil || result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted {
			t.Fatalf("result: %+v", result)
		}
	})
}
