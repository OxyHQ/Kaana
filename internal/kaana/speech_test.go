package kaana_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
)

func TestSpeechExecutorFramesAudioAndSettlesCharacters(t *testing.T) {
	data := append([]byte("ID3\x04"), bytes.Repeat([]byte{0, 255, 9}, 40000)...)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/tts" || r.Header.Get("Authorization") != "Bearer speech-executor-test-secret" {
			t.Error("wrong speech transport")
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(data)
	}))
	defer upstream.Close()
	adapter, err := openaicompat.New(openaicompat.Config{Provider: "xai", BaseURL: upstream.URL + "/v1", Declarations: []provider.KeyDeclaration{{KeyID: "xai-test", Secret: "speech-executor-test-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := baseRequest()
	reference := contract.ModelReference("x-ai/text-to-speech@observed-2026-09-13")
	text := "Hola 👋"
	request.Target.ModelReference = &reference
	request.Modality = contract.ModalityAudio
	request.Stream = false
	request.Client.APIFormat = contract.APIFormatAudioSpeech
	request.Client.Endpoint = "/v1/audio/speech"
	request.Input = contract.Input{Format: contract.InputText, Text: &text}
	request.Speech = &contract.SpeechParameters{Voice: "female", ResponseFormat: "mp3"}
	request.AuthorizedRoutes = []contract.AuthorizedRoute{{Substitution: contract.SubstitutionSameModel, DeploymentID: "speech-exact", ModelReference: reference, Provider: "xai", Regions: []contract.Region{}}}
	events, result := (harness{deployments: `{"deploymentId":"speech-exact","provider":"xai","modelReference":"x-ai/text-to-speech@observed-2026-09-13","upstreamModelId":"tts","regions":[],"current":true}`, adapters: []provider.Adapter{adapter}}).run(t, request)
	if calls.Load() != 1 || result.Failure != nil || result.Report == nil || result.Report.Outcome != contract.OutcomeCompleted {
		t.Fatalf("speech result: %+v, upstream calls %d", result, calls.Load())
	}
	want := []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: 7}}
	if !reflect.DeepEqual(result.Report.Units, want) || result.Report.UsageSource != contract.UsageOxyMeasured {
		t.Fatalf("wrong settlement: %+v", result.Report)
	}
	if len(events) != 6 || events[0].EventType() != contract.EventStart || events[len(events)-1].EventType() != contract.EventDone {
		t.Fatalf("unexpected audio framing: %v", events)
	}
	var decoded []byte
	chunks := 0
	for _, event := range events {
		if audio, ok := event.(*contract.StreamAudioEvent); ok {
			if audio.RequestID != request.Attribution.RequestID || audio.Seq != chunks+2 || audio.OutputIndex != 0 || audio.MediaType != "audio/mpeg" {
				t.Fatalf("wrong audio frame: %+v", audio)
			}
			part, err := base64.StdEncoding.DecodeString(audio.Data)
			if err != nil {
				t.Fatal(err)
			}
			decoded = append(decoded, part...)
			chunks++
		}
	}
	if chunks != 3 || !bytes.Equal(decoded, data) {
		t.Fatal("audio did not survive executor framing")
	}
}
