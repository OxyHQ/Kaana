package openaicompat

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

type transcriptCapture struct {
	started bool
	text    strings.Builder
	units   []contract.UsageQuantity
}

func (c *transcriptCapture) Start(contract.ModelReference, time.Time) error {
	c.started = true
	return nil
}
func (c *transcriptCapture) Delta(index int, channel contract.DeltaChannel, text string) error {
	if !c.started || index != 0 || channel != contract.ChannelOutputText {
		return errors.New("transcript delta out of order or on the wrong channel")
	}
	c.text.WriteString(text)
	return nil
}
func (c *transcriptCapture) ToolCall(provider.ToolCallDelta) error {
	return errors.New("unexpected tool call")
}
func (c *transcriptCapture) Usage(units []contract.UsageQuantity, _ contract.UsageSource) error {
	c.units = units
	return nil
}

var transcriptionAudio = []byte("\x1aE\xdf\xa3 webm bytes")

func transcriptionFixture() (*contract.Request, provider.Route) {
	r := requestWith(nil)
	r.Modality = contract.ModalityAudio
	r.Stream = false
	mediaType := "audio/webm"
	data := base64.StdEncoding.EncodeToString(transcriptionAudio)
	r.Input = contract.Input{Format: contract.InputMessages, Messages: []contract.Message{{
		Role: contract.RoleUser,
		Content: []contract.ContentPart{{
			Type:   contract.ContentPartAudio,
			Source: &contract.ContentSource{Kind: contract.ContentSourceInline, MediaType: &mediaType, Data: &data},
		}},
	}}}
	r.Client.APIFormat = contract.APIFormatAudioTranscriptions
	language := contract.LanguageTag("es-ES")
	r.Transcription = &contract.TranscriptionParameters{Language: &language}
	route := testRoute()
	route.Provider = "xai"
	route.ModelReference = "x-ai/grok-voice-transcribe-2.0@observed-2026-10-16"
	route.UpstreamModelID = "grok-voice-transcribe-2.0"
	return r, route
}

// The upload is what xAI documents: the model, the caller's language, and the
// audio as the last field. The duration xAI reports is the billed quantity.
func TestTranscriptionRealWireSendsTheUploadAndMetersTheReportedDuration(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/stt" || r.Header.Get("Authorization") != "Bearer "+fakeAPIKey {
			t.Errorf("wrong transcription transport: %s %s", r.Method, r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		var names []string
		values := map[string]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(part)
			names = append(names, part.FormName())
			values[part.FormName()] = string(body)
			if part.FormName() == "file" && (part.Header.Get("Content-Type") != "audio/webm" || part.FileName() != "audio.webm") {
				t.Errorf("file part = %v", part.Header)
			}
		}
		if strings.Join(names, ",") != "model,language,file" {
			t.Errorf("fields = %v; the file must be last", names)
		}
		if values["model"] != "grok-voice-transcribe-2.0" || values["language"] != "es-ES" || values["file"] != string(transcriptionAudio) {
			t.Errorf("values = %#v", values)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hola mundo","language":"es","duration":3.4501,"words":[{"text":"hola","start":0.2,"end":0.5}]}`)
	}))
	defer server.Close()
	a := speechAdapter(t, server.URL+"/v1")
	request, route := transcriptionFixture()
	call, err := a.Translate(request, route)
	if err != nil {
		t.Fatal(err)
	}
	capture := &transcriptCapture{}
	outcome, err := a.Stream(context.Background(), call, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || capture.text.String() != "hola mundo" {
		t.Fatalf("transcript = %q after %d calls", capture.text.String(), calls)
	}
	expected := []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: 3451}}
	if !reflect.DeepEqual(outcome.Units, expected) || !reflect.DeepEqual(capture.units, expected) || outcome.UsageSource != contract.UsageProviderReported {
		t.Fatalf("transcription metering = %+v", outcome)
	}
	// xAI names the language it heard as a tag, which the response carries.
	if outcome.DetectedLanguage == nil || *outcome.DetectedLanguage != "es" {
		t.Errorf("detected language = %v, want es", outcome.DetectedLanguage)
	}
}

// groqTranscriptionFixture is a Whisper request on Groq's slug.
func groqTranscriptionFixture() (*contract.Request, provider.Route) {
	r, route := transcriptionFixture()
	prompt := "names: Kaana, Alia"
	r.Transcription = &contract.TranscriptionParameters{Prompt: &prompt}
	route.Provider = "groq"
	route.ModelReference = "openai/whisper-large-v3-turbo@observed-2026-10-16"
	route.UpstreamModelID = "whisper-large-v3-turbo"
	return r, route
}

// Groq serves Whisper on OpenAI's endpoint: the model, verbose_json (whose
// duration is what it bills), the caller's prompt, and the audio. Groq bills
// at least ten seconds, so a 2.5 second clip is metered as ten; its
// verbose_json names the language ("spanish"), which is not a tag and is not
// passed off as one.
func TestGroqTranscribesWhisperOnOpenAIsEndpointAndBillsItsMinimum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/openai/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer "+fakeAPIKey {
			t.Errorf("wrong transcription transport: %s %s", r.Method, r.URL.Path)
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		reader := multipart.NewReader(r.Body, params["boundary"])
		values := map[string]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(part)
			values[part.FormName()] = string(body)
		}
		want := map[string]string{"model": "whisper-large-v3-turbo", "response_format": "verbose_json", "prompt": "names: Kaana, Alia", "file": string(transcriptionAudio)}
		if !reflect.DeepEqual(values, want) {
			t.Errorf("upload = %#v, want %#v", values, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"task":"transcribe","language":"spanish","duration":2.5,"text":"hola","segments":[]}`)
	}))
	defer server.Close()
	a, err := New(Config{Provider: "groq", BaseURL: server.URL + "/openai/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatal(err)
	}
	request, route := groqTranscriptionFixture()
	call, err := a.Translate(request, route)
	if err != nil {
		t.Fatal(err)
	}
	capture := &transcriptCapture{}
	outcome, err := a.Stream(context.Background(), call, capture, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capture.text.String() != "hola" {
		t.Errorf("transcript = %q", capture.text.String())
	}
	if want := []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: 10_000}}; !reflect.DeepEqual(outcome.Units, want) {
		t.Errorf("units = %v, want Groq's ten-second minimum %v", outcome.Units, want)
	}
	if outcome.DetectedLanguage != nil {
		t.Errorf("a language name was published as a tag: %v", *outcome.DetectedLanguage)
	}
}

// What Groq's statement does not take is refused before anything is sent: a
// named language (Groq publishes no list; the model detects it) and an
// upload it does not read.
func TestGroqRefusesWhatItsStatementDoesNotTake(t *testing.T) {
	a, err := New(Config{Provider: "groq", BaseURL: "https://upstream.invalid/openai/v1", Declarations: provider.DeclareKeys([]string{fakeAPIKey})})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*contract.Request){
		"a language": func(r *contract.Request) {
			language := contract.LanguageTag("es")
			r.Transcription.Language = &language
		},
		"aac": func(r *contract.Request) {
			aac := "audio/aac"
			r.Input.Messages[0].Content[0].Source.MediaType = &aac
		},
	} {
		request, route := groqTranscriptionFixture()
		mutate(request)
		var refusal provider.ErrUnsupported
		if _, err := a.Translate(request, route); !errors.As(err, &refusal) || refusal.Code != contract.CodeInvalidRequest {
			t.Errorf("%s: Translate = %v, want invalid_request", name, err)
		}
	}
}

// xAI takes no prompt: one is refused, never dropped.
func TestXAIRefusesAPrompt(t *testing.T) {
	a := speechAdapter(t, "https://upstream.invalid/v1")
	request, route := transcriptionFixture()
	prompt := "names"
	request.Transcription.Prompt = &prompt
	var refusal provider.ErrUnsupported
	if _, err := a.Translate(request, route); !errors.As(err, &refusal) || refusal.Param != "transcription.prompt" {
		t.Errorf("Translate = %v, want a refusal naming transcription.prompt", err)
	}
}

// Without a language the field is absent: the model detects it.
func TestTranscriptionWithoutALanguageSendsNone(t *testing.T) {
	a := speechAdapter(t, "https://upstream.invalid/v1")
	request, route := transcriptionFixture()
	request.Transcription = nil
	call, err := a.Translate(request, route)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(call.Body), `name="language"`) {
		t.Error("a language was invented for a caller who named none")
	}
}

func TestTranscriptionRefusesWhatTheUploadCannotCarry(t *testing.T) {
	cases := map[string]func(*contract.Request, *provider.Route){
		"stream":        func(r *contract.Request, _ *provider.Route) { r.Stream = true },
		"text modality": func(r *contract.Request, _ *provider.Route) { r.Modality = contract.ModalityText },
		"two parts": func(r *contract.Request, _ *provider.Route) {
			r.Input.Messages[0].Content = append(r.Input.Messages[0].Content, r.Input.Messages[0].Content[0])
		},
		"remote audio": func(r *contract.Request, _ *provider.Route) {
			r.Input.Messages[0].Content[0].Source.Kind = contract.ContentSourceURL
		},
		"pcm": func(r *contract.Request, _ *provider.Route) {
			pcm := "audio/pcm"
			r.Input.Messages[0].Content[0].Source.MediaType = &pcm
		},
		"not base64": func(r *contract.Request, _ *provider.Route) {
			bad := "%%%"
			r.Input.Messages[0].Content[0].Source.Data = &bad
		},
		"sampling": func(r *contract.Request, _ *provider.Route) {
			temperature := 0.2
			r.Sampling.Temperature = &temperature
		},
		"chat deployment": func(_ *contract.Request, route *provider.Route) { route.UpstreamModelID = "grok-4.7" },
		"speech endpoint": func(_ *contract.Request, route *provider.Route) { route.UpstreamModelID = "tts" },
	}
	a := speechAdapter(t, "https://upstream.invalid/v1")
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, route := transcriptionFixture()
			mutate(r, &route)
			_, err := a.Translate(r, route)
			var refusal provider.ErrUnsupported
			if !errors.As(err, &refusal) {
				t.Fatalf("expected a refusal before transport, got %v", err)
			}
		})
	}
}

// A chat request can never reach the transcription deployment.
func TestTheTranscriptionDeploymentRefusesChat(t *testing.T) {
	a := speechAdapter(t, "https://upstream.invalid/v1")
	_, route := transcriptionFixture()
	_, err := a.Translate(requestWith(nil), route)
	var refusal provider.ErrUnsupported
	if !errors.As(err, &refusal) || refusal.Code != contract.CodeUnsupportedModality {
		t.Fatalf("chat on the transcription deployment = %v", err)
	}
}

// A response without a duration cannot be billed, so it is not believed.
func TestTranscriptionRefusesAResponseWithoutADuration(t *testing.T) {
	for name, body := range map[string]string{
		"no duration":   `{"text":"hola","language":"es"}`,
		"zero duration": `{"text":"hola","language":"es","duration":0}`,
		"no text":       `{"language":"es","duration":1.2}`,
		"not json":      `hola`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			a := speechAdapter(t, server.URL+"/v1")
			request, route := transcriptionFixture()
			call, err := a.Translate(request, route)
			if err != nil {
				t.Fatal(err)
			}
			capture := &transcriptCapture{}
			_, err = a.Stream(context.Background(), call, capture, nil)
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || capture.started {
				t.Fatalf("invalid response = %v (started %v)", err, capture.started)
			}
		})
	}
}
