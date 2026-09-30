package kaana_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaiaudio"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

const audioChatDeployment = `{"deploymentId":"dep_audio_chat","provider":"openai-audio","modelReference":"openai/gpt-audio-1.5@observed-2026-09-30","upstreamModelId":"gpt-audio-1.5","regions":[],"current":true}`

type redirectTransport struct{ target string }

// RoundTrip keeps the adapter's reviewed destination on the request and only
// delivers it to the local fake.
func (r redirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme, copy.URL.Host = "http", strings.TrimPrefix(r.target, "http://")
	return http.DefaultTransport.RoundTrip(copy)
}

func spokenChatRequest() *contract.Request {
	request := baseRequest()
	reference := contract.ModelReference("openai/gpt-audio-1.5@observed-2026-09-30")
	request.Target.ModelReference = &reference
	request.Modality = contract.ModalityAudio
	request.Client.APIFormat = contract.APIFormatChatCompletions
	request.Client.Endpoint = "/v1/chat/completions"
	request.AudioOutput = &contract.AudioOutputParameters{Voice: "marin", Format: contract.AudioOutputPCM}
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{Substitution: contract.SubstitutionSameModel, DeploymentID: "dep_audio_chat", ModelReference: reference, Provider: "openai-audio", Regions: []contract.Region{}}}
	return request
}

func TestSpokenChatExecutesThroughTheAudioAdapterAndSettlesAudioTokens(t *testing.T) {
	audio := bytes.Repeat([]byte{1, 2, 3, 4}, 20000) // 80000 bytes: two contract chunks
	frame := func(value any) string {
		encoded, _ := json.Marshal(value)
		return "data: " + string(encoded) + "\n\n"
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer audio-chat-executor-test-secret" {
			t.Error("wrong audio chat transport")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"audio": map[string]any{"id": "audio_1", "transcript": "Hola", "data": base64.StdEncoding.EncodeToString(audio)}}}}}))
		_, _ = io.WriteString(w, frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":250,"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":60,"cached_tokens_details":{"audio_tokens":5}},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":200}}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	adapter, err := openaiaudio.New(openaiaudio.Config{
		Declarations: []provider.KeyDeclaration{{KeyID: "openai-audio-test", Secret: "audio-chat-executor-test-secret"}},
		HTTPClient:   &http.Client{Transport: redirectTransport{target: upstream.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Parse([]byte(`{"schemaVersion":1,"rateCardVersionId":"rc_audio_chat","source":"operator","sourceVersion":"test-fixture","observedAt":"2026-09-30T00:00:00Z","effectiveAt":"2026-09-30T00:00:00Z","rateCards":[{"deploymentId":"dep_audio_chat","currency":"XTS","rates":[
		{"unit":"requests","amountPerUnit":0},
		{"unit":"input_tokens","amountPerUnit":1},
		{"unit":"cached_input_tokens","amountPerUnit":2},
		{"unit":"audio_input_tokens","amountPerUnit":3},
		{"unit":"cached_audio_input_tokens","amountPerUnit":4},
		{"unit":"output_tokens","amountPerUnit":5},
		{"unit":"audio_output_tokens","amountPerUnit":6}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request := spokenChatRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("the fixture is not a valid envelope: %v", err)
	}
	events, result := (harness{deployments: audioChatDeployment, adapters: []provider.Adapter{adapter}, costs: cards}).run(t, request)
	if result.Failure != nil || result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted {
		t.Fatalf("result: %+v", result)
	}
	want := []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1},
		{Unit: contract.UnitInputTokens, Quantity: 25},
		{Unit: contract.UnitCachedInputTokens, Quantity: 15},
		{Unit: contract.UnitAudioInputTokens, Quantity: 55},
		{Unit: contract.UnitCachedAudioInputTokens, Quantity: 5},
		{Unit: contract.UnitOutputTokens, Quantity: 50},
		{Unit: contract.UnitAudioOutputTokens, Quantity: 200},
	}
	if !reflect.DeepEqual(result.Report.Units, want) || result.Report.UsageSource != contract.UsageProviderReported {
		t.Fatalf("settled %+v (%s), want %+v", result.Report.Units, result.Report.UsageSource, want)
	}
	if result.Report.TimeToFirstTokenMs == nil {
		t.Error("the first spoken output set no time to first token")
	}
	// 25*1 + 15*2 + 55*3 + 5*4 + 50*5 + 200*6 = 1690: every audio unit priced
	// at its own rate, none folded into a text unit.
	if len(result.UpstreamCost.Totals) != 1 || result.UpstreamCost.Totals[0].Amount != 1690 {
		t.Errorf("upstream cost = %+v", result.UpstreamCost)
	}
	if len(result.UpstreamCost.Attempts) != 1 || !result.UpstreamCost.Attempts[0].Served {
		t.Errorf("the served attempt is not marked served: %+v", result.UpstreamCost.Attempts)
	}

	var kinds []string
	var decoded []byte
	for _, event := range events {
		switch e := event.(type) {
		case *contract.StreamDeltaEvent:
			if e.Channel != contract.ChannelOutputAudioTranscript || e.Text != "Hola" {
				t.Errorf("delta on %s: %q", e.Channel, e.Text)
			}
		case *contract.StreamAudioEvent:
			if e.MediaType != "audio/pcm" {
				t.Errorf("audio media type %q", e.MediaType)
			}
			part, err := base64.StdEncoding.DecodeString(e.Data)
			if err != nil || len(part) > 49152 {
				t.Errorf("audio chunk of %d bytes: %v", len(part), err)
			}
			decoded = append(decoded, part...)
		}
		kinds = append(kinds, string(event.EventType()))
	}
	if strings.Join(kinds, ",") != "start,delta,audio,audio,usage,done" {
		t.Errorf("event sequence = %v", kinds)
	}
	if !bytes.Equal(decoded, audio) {
		t.Error("the spoken audio did not survive executor framing")
	}
}

// TestSpokenOutputWithoutUsageNeverEstimatesAudioTokens covers the fallback:
// audio has no honest reconstruction from transport bytes, so an answer whose
// provider reported nothing settles as an estimate of text units only — the
// transcript as output text — and never invents an audio-token count.
func TestSpokenOutputWithoutUsageNeverEstimatesAudioTokens(t *testing.T) {
	for name, transcript := range map[string]string{"with transcript": "Hola mundo", "audio only": ""} {
		t.Run(name, func(t *testing.T) {
			adapter := speaking{&scriptedAdapter{slug: "openai-audio", stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
					return provider.Outcome{}, err
				}
				if transcript != "" {
					if err := out.Delta(0, contract.ChannelOutputAudioTranscript, transcript); err != nil {
						return provider.Outcome{}, err
					}
				}
				if err := out.(provider.AudioEmitter).Audio(0, "audio/pcm", make([]byte, 4800)); err != nil {
					return provider.Outcome{}, err
				}
				return provider.Outcome{FinishReason: contract.FinishStop}, nil
			}}}
			_, result := (harness{deployments: audioChatDeployment, adapters: []provider.Adapter{adapter}}).run(t, spokenChatRequest())
			if result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted || result.Report.UsageSource != contract.UsageEstimated {
				t.Fatalf("result: %+v", result)
			}
			reported := make(map[contract.UsageUnit]int)
			for _, quantity := range result.Report.Units {
				reported[quantity.Unit] = quantity.Quantity
			}
			for _, invented := range []contract.UsageUnit{
				contract.UnitAudioInputTokens, contract.UnitCachedAudioInputTokens, contract.UnitAudioOutputTokens,
				contract.UnitAudioInputMilliseconds, contract.UnitAudioOutputMilliseconds,
			} {
				if _, ok := reported[invented]; ok {
					t.Errorf("the estimate invented %s: %v", invented, result.Report.Units)
				}
			}
			if reported[contract.UnitRequests] != 1 || reported[contract.UnitInputTokens] <= 0 {
				t.Errorf("estimate = %v", result.Report.Units)
			}
			if got := reported[contract.UnitOutputTokens]; (transcript != "") != (got > 0) {
				t.Errorf("output tokens = %d for transcript %q", got, transcript)
			}
			if result.Report.TimeToFirstTokenMs == nil {
				t.Error("delivered audio set no time to first token")
			}
		})
	}
}
