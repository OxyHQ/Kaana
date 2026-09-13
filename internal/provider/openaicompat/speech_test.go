package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type speechCapture struct {
	started bool
	audio   []byte
	chunks  int
	units   []contract.UsageQuantity
	fail    error
}

func (s *speechCapture) Start(contract.ModelReference, time.Time) error { s.started = true; return nil }
func (s *speechCapture) Delta(int, contract.DeltaChannel, string) error {
	return errors.New("unexpected text")
}
func (s *speechCapture) ToolCall(provider.ToolCallDelta) error { return errors.New("unexpected tool") }
func (s *speechCapture) Usage(units []contract.UsageQuantity, _ contract.UsageSource) error {
	s.units = units
	return nil
}
func (s *speechCapture) Audio(index int, mediaType string, data []byte) error {
	if !s.started || index != 0 || mediaType != "audio/mpeg" || len(data) > 49152 {
		return errors.New("invalid audio ordering or metadata")
	}
	s.chunks++
	if s.fail != nil {
		return s.fail
	}
	s.audio = append(s.audio, data...)
	return nil
}
func speechFixture() (*contract.Request, provider.Route) {
	r := requestWith(nil)
	r.Modality = contract.ModalityAudio
	r.Stream = false
	text := "Hola 👋"
	r.Input = contract.Input{Format: contract.InputText, Text: &text}
	r.Client.APIFormat = contract.APIFormatAudioSpeech
	speed := 1.15
	r.Speech = &contract.SpeechParameters{Voice: "female", ResponseFormat: "mp3", Speed: &speed}
	route := testRoute()
	route.Provider = "xai"
	route.ModelReference = "xai/tts@observed-2026-09-13"
	route.UpstreamModelID = "tts"
	return r, route
}
func speechAdapter(t *testing.T, url string) *Adapter {
	t.Helper()
	a, err := New(Config{Provider: "xai", BaseURL: url, Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestSpeechRealWirePreservesParametersAudioAndCharacterUnits(t *testing.T) {
	data := append([]byte("ID3\x04"), bytes.Repeat([]byte{1, 255, 0}, 40000)...)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1/tts" || r.Header.Get("Authorization") != "Bearer "+fakeAPIKey {
			t.Error("wrong speech transport")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		expected := map[string]any{"text": "Hola 👋", "voice_id": "eve", "language": "auto", "speed": 1.15,
			"output_format": map[string]any{"codec": "mp3", "sample_rate": float64(24000), "bit_rate": float64(128000)}}
		if !reflect.DeepEqual(body, expected) {
			t.Errorf("speech body = %#v", body)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	a := speechAdapter(t, server.URL+"/v1")
	request, route := speechFixture()
	call, err := a.Translate(request, route)
	if err != nil {
		t.Fatal(err)
	}
	capture := &speechCapture{}
	outcome, err := a.Stream(context.Background(), call, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || capture.chunks != 3 || !bytes.Equal(capture.audio, data) {
		t.Fatal("speech bytes or chunk boundaries lost")
	}
	expected := []contract.UsageQuantity{{Unit: contract.UnitCharacters, Quantity: 7}}
	if !reflect.DeepEqual(outcome.Units, expected) || !reflect.DeepEqual(capture.units, expected) || outcome.UsageSource != contract.UsageOxyMeasured {
		t.Fatalf("speech metering = %+v", outcome)
	}
}
func TestSpeechTranslationRefusesUnsupportedParametersBeforeTransport(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*contract.Request)
	}{
		{"missing", func(r *contract.Request) { r.Speech = nil }},
		{"stream", func(r *contract.Request) { r.Stream = true }},
		{"text modality", func(r *contract.Request) { r.Modality = contract.ModalityText }},
		{"empty", func(r *contract.Request) { *r.Input.Text = "" }},
		{"too long", func(r *contract.Request) { *r.Input.Text = strings.Repeat("a", 15001) }},
		{"unsupported format", func(r *contract.Request) { r.Speech.ResponseFormat = "flac" }},
		{"unsupported voice", func(r *contract.Request) { r.Speech.Voice = "unknown" }},
		{"slow", func(r *contract.Request) { *r.Speech.Speed = 0.6 }},
		{"fast", func(r *contract.Request) { *r.Speech.Speed = 1.6 }},
	}
	a := speechAdapter(t, "https://upstream.invalid/v1")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, route := speechFixture()
			tc.mutate(r)
			_, err := a.Translate(r, route)
			var refusal provider.ErrUnsupported
			if !errors.As(err, &refusal) {
				t.Fatalf("expected preflight refusal, got %v", err)
			}
		})
	}
	r, route := speechFixture()
	r.Speech.Voice = "male"
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	var body speechRequest
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Voice != "rex" {
		t.Fatal("male voice was not translated")
	}
}
func TestSpeechDoesNotExposeEmptyJsonOrOversizedSuccessAsAudio(t *testing.T) {
	for _, tc := range []struct {
		name, media string
		data        []byte
	}{
		{"empty", "audio/mpeg", nil}, {"json", "application/json", []byte(`{"error":"no audio"}`)},
		{"wrong bytes", "audio/mpeg", []byte("not an mp3")},
		{"oversized", "audio/mpeg", append([]byte("ID3"), make([]byte, maxSpeechAudioBytes)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				_, _ = w.Write(tc.data)
			}))
			defer server.Close()
			a := speechAdapter(t, server.URL+"/v1")
			r, route := speechFixture()
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			capture := &speechCapture{}
			outcome, err := a.Stream(context.Background(), call, capture, nil)
			if err == nil || capture.started || len(capture.audio) != 0 || len(outcome.Units) != 0 {
				t.Fatal("invalid upstream audio was presented as successful output")
			}
		})
	}
}
func TestSpeechStopsOnDownstreamFailureAndRetainsMeasuredUnits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(append([]byte("ID3"), make([]byte, 100000)...))
	}))
	defer server.Close()
	a := speechAdapter(t, server.URL+"/v1")
	r, route := speechFixture()
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("downstream closed")
	capture := &speechCapture{fail: failure}
	outcome, err := a.Stream(context.Background(), call, capture, nil)
	if !errors.Is(err, failure) || capture.chunks != 1 || len(outcome.Units) != 1 {
		t.Fatalf("failure lost cancellation or units: %+v %v", outcome, err)
	}
}
func TestSpeechCancellationReachesUpstream(t *testing.T) {
	arrived := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(arrived)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	a := speechAdapter(t, server.URL+"/v1")
	r, route := speechFixture()
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := a.Stream(ctx, call, &speechCapture{}, nil); finished <- err }()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never received request")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter did not finish")
	}
}

func TestSpeechCannotCrossAnAuthorizedDeploymentModality(t *testing.T) {
	a := speechAdapter(t, "https://api.x.ai/v1")
	request, route := speechFixture()
	if _, err := a.Translate(request, route); err != nil {
		t.Fatal(err)
	}
	route.UpstreamModelID = "grok-4"
	if _, err := a.Translate(request, route); err == nil {
		t.Fatal("chat deployment accepted speech")
	}
	request, route = speechFixture()
	request.Client.APIFormat = contract.APIFormatChatCompletions
	request.Modality = contract.ModalityText
	request.Speech = nil
	if _, err := a.Translate(request, route); err == nil {
		t.Fatal("speech deployment accepted chat")
	}
}
