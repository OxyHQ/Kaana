package openaiaudio

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const fakeKey = "sk-openai-audio-synthetic-credential-not-valid"

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// adapter points the real client at a local fake while asserting the request
// left for OpenAI's own origin with this key: the fake is the wire, the
// destination is still the reviewed one.
func adapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a, err := New(Config{Declarations: provider.DeclareKeys([]string{fakeKey}), HTTPClient: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer "+fakeKey {
			t.Error("wrong credential or destination")
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = "http", strings.TrimPrefix(server.URL, "http://")
		return http.DefaultTransport.RoundTrip(copy)
	})}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type capture struct {
	started bool
	text    string
	units   []contract.UsageQuantity
	fail    error
}

func (c *capture) Start(contract.ModelReference, time.Time) error { c.started = true; return nil }
func (c *capture) Delta(_ int, channel contract.DeltaChannel, text string) error {
	if !c.started || channel != contract.ChannelOutputText {
		return errors.New("invalid transcript event")
	}
	c.text += text
	return c.fail
}
func (c *capture) ToolCall(provider.ToolCallDelta) error { return errors.New("unexpected tool") }
func (c *capture) Usage(u []contract.UsageQuantity, _ contract.UsageSource) error {
	if !c.started {
		return errors.New("usage before start")
	}
	c.units = u
	return nil
}

func transcriptionRequest(model, mediaType string, audio []byte) (*contract.Request, provider.Route) {
	data := base64.StdEncoding.EncodeToString(audio)
	part := contract.ContentPart{
		Type:   contract.ContentPartAudio,
		Source: &contract.ContentSource{Kind: contract.ContentSourceInline, Data: &data, MediaType: &mediaType},
	}
	request := &contract.Request{
		Modality: contract.ModalityAudio,
		Client:   contract.ClientRequestMetadata{APIFormat: contract.APIFormatAudioTranscriptions},
		Input: contract.Input{
			Format:   contract.InputMessages,
			Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{part}}},
		},
	}
	route := provider.Route{
		Provider: Slug, UpstreamModelID: model, DeploymentID: "dep_openai_audio_test",
		ModelReference: contract.ModelReference("openai/" + model + "@observed-2026-09-30"),
	}
	return request, route
}

// readUpload parses the request the fake received exactly as OpenAI would: a
// multipart form whose file part is named for its container.
func readUpload(t *testing.T, r *http.Request) (fields map[string]string, fileName, fileType string, audio []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("upload is %q, not multipart", r.Header.Get("Content-Type"))
	}
	fields = make(map[string]string)
	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return fields, fileName, fileType, audio
		}
		if err != nil {
			t.Fatalf("malformed multipart upload: %v", err)
		}
		body, _ := io.ReadAll(part)
		if part.FormName() == "file" {
			fileName, fileType, audio = part.FileName(), part.Header.Get("Content-Type"), body
			continue
		}
		if _, duplicate := fields[part.FormName()]; duplicate {
			t.Fatalf("field %q sent twice", part.FormName())
		}
		fields[part.FormName()] = string(body)
	}
}

func TestTranscriptionUploadAndReportedUsage(t *testing.T) {
	cases := []struct {
		name, model, format, response string
		want                          []contract.UsageQuantity
	}{
		{
			name: "token usage", model: "gpt-4o-transcribe", format: "json",
			response: `{"text":"Hola mundo","usage":{"type":"tokens","input_tokens":14,"input_token_details":{"audio_tokens":12,"text_tokens":2},"output_tokens":45,"total_tokens":59}}`,
			want:     []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 14}, {Unit: contract.UnitOutputTokens, Quantity: 45}},
		},
		{
			name: "duration usage", model: "gpt-transcribe", format: "json",
			response: `{"text":"Hola mundo","usage":{"type":"duration","seconds":3.2001}}`,
			want:     []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: 3201}},
		},
		{
			name: "whisper verbose duration", model: "whisper-1", format: "verbose_json",
			response: `{"task":"transcribe","language":"spanish","duration":8.47,"text":"Hola mundo","segments":[]}`,
			want:     []contract.UsageQuantity{{Unit: contract.UnitAudioInputMilliseconds, Quantity: 8470}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			audio := []byte("RIFF\x00\x00\x00\x00WAVEfmt synthetic")
			calls := 0
			a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" || r.URL.RawQuery != "" {
					t.Errorf("wrong endpoint %s %s", r.Method, r.URL)
				}
				fields, fileName, fileType, got := readUpload(t, r)
				if len(fields) != 2 || fields["model"] != c.model || fields["response_format"] != c.format {
					t.Errorf("upload fields = %v; an unset control must not be invented", fields)
				}
				if fileName != "audio.wav" || fileType != "audio/wav" || string(got) != string(audio) {
					t.Errorf("file part = %q %q %q", fileName, fileType, got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, c.response)
			})
			r, route := transcriptionRequest(c.model, "audio/wav", audio)
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			out := &capture{}
			outcome, err := a.Stream(context.Background(), call, out, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || out.text != "Hola mundo" || outcome.FinishReason != contract.FinishStop || outcome.UsageSource != contract.UsageProviderReported {
				t.Fatalf("calls=%d text=%q outcome=%+v", calls, out.text, outcome)
			}
			if len(outcome.Units) != len(c.want) || len(out.units) != len(c.want) {
				t.Fatalf("units = %+v, emitted %+v, want %+v", outcome.Units, out.units, c.want)
			}
			for i := range c.want {
				if outcome.Units[i] != c.want[i] || out.units[i] != c.want[i] {
					t.Errorf("unit %d = %+v, want %+v", i, outcome.Units[i], c.want[i])
				}
			}
			if outcome.KeyID == "" {
				t.Error("the attempt names no key, so its cost cannot be attributed")
			}
		})
	}
}

func TestMeasuredUsageSurvivesADownstreamFailure(t *testing.T) {
	a := adapter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"text":"Hola","usage":{"type":"duration","seconds":2}}`)
	})
	r, route := transcriptionRequest("gpt-transcribe", "audio/mpeg", []byte("ID3synthetic"))
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	sink := &capture{fail: errors.New("sink closed")}
	outcome, err := a.Stream(context.Background(), call, sink, nil)
	if !errors.Is(err, sink.fail) {
		t.Fatalf("err = %v", err)
	}
	if len(outcome.Units) != 1 || outcome.Units[0].Quantity != 2000 {
		t.Fatalf("billed audio was lost with the sink: %+v", outcome.Units)
	}
}

func TestAResponseWithoutUsageOrTranscriptIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"no usage":               `{"text":"Hola"}`,
		"unknown usage type":     `{"text":"Hola","usage":{"type":"characters","count":4}}`,
		"inconsistent tokens":    `{"text":"Hola","usage":{"type":"tokens","input_tokens":1,"output_tokens":2,"total_tokens":9}}`,
		"negative tokens":        `{"text":"Hola","usage":{"type":"tokens","input_tokens":-1,"output_tokens":2}}`,
		"zero duration":          `{"text":"Hola","usage":{"type":"duration","seconds":0}}`,
		"no transcript":          `{"usage":{"type":"duration","seconds":1}}`,
		"not json":               `<html>`,
		"oversized response":     `{"text":"` + strings.Repeat("a", maxResponseBytes) + `"}`,
		"no duration on whisper": `{"text":"Hola","segments":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := adapter(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			r, route := transcriptionRequest("whisper-1", "audio/flac", []byte("fLaCsynthetic"))
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			out := &capture{}
			_, err = a.Stream(context.Background(), call, out, nil)
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || upstream.Code != contract.CodeProviderError {
				t.Fatalf("err = %v", err)
			}
			if out.started {
				t.Fatal("an invalid response started the customer's stream")
			}
		})
	}
	// Positive control: the same whisper shape with its duration is accepted.
	a := adapter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"text":"Hola","duration":1,"segments":[]}`)
	})
	r, route := transcriptionRequest("whisper-1", "audio/flac", []byte("fLaCsynthetic"))
	call, _ := a.Translate(r, route)
	if _, err := a.Stream(context.Background(), call, &capture{}, nil); err != nil {
		t.Fatalf("a valid whisper response was refused: %v", err)
	}
}

func TestContainerAndSizeBoundsAreEnforcedBeforeSending(t *testing.T) {
	a := adapter(t, func(http.ResponseWriter, *http.Request) { t.Error("a refused request reached the upstream") })
	accepted := map[string]string{
		"audio/wav": "audio.wav", "audio/x-wav": "audio.wav", "audio/mpeg": "audio.mp3", "audio/flac": "audio.flac",
		"audio/ogg": "audio.ogg", "audio/webm": "audio.webm", "audio/mp4": "audio.m4a",
	}
	for mediaType, name := range accepted {
		r, route := transcriptionRequest("gpt-transcribe", mediaType, []byte("synthetic"))
		call, err := a.Translate(r, route)
		if err != nil || !strings.Contains(string(call.Body), `filename="`+name+`"`) {
			t.Errorf("%s: %v", mediaType, err)
		}
	}
	for _, mediaType := range []string{"audio/pcm", "audio/aac", "video/mp4", "text/plain"} {
		r, route := transcriptionRequest("gpt-transcribe", mediaType, []byte("synthetic"))
		if _, err := a.Translate(r, route); err == nil {
			t.Errorf("%s accepted", mediaType)
		}
	}
	limit, route := transcriptionRequest("gpt-transcribe", "audio/wav", make([]byte, maxAudioBytes))
	if _, err := a.Translate(limit, route); err != nil {
		t.Fatalf("audio at the 20 MiB ceiling refused: %v", err)
	}
	over, route := transcriptionRequest("gpt-transcribe", "audio/wav", make([]byte, maxAudioBytes+1))
	if _, err := a.Translate(over, route); err == nil {
		t.Fatal("audio over 20 MiB accepted")
	}
	empty, route := transcriptionRequest("gpt-transcribe", "audio/wav", nil)
	if _, err := a.Translate(empty, route); err == nil {
		t.Fatal("empty audio accepted")
	}
}

func TestUnexpressibleRequestsAreRefusedBeforeSending(t *testing.T) {
	a := adapter(t, func(http.ResponseWriter, *http.Request) { t.Error("a refused request reached the upstream") })
	for name, mutate := range map[string]func(*contract.Request, *provider.Route){
		"streaming":     func(r *contract.Request, _ *provider.Route) { r.Stream = true },
		"chat format":   func(r *contract.Request, _ *provider.Route) { r.Client.APIFormat = contract.APIFormatChatCompletions },
		"text modality": func(r *contract.Request, _ *provider.Route) { r.Modality = contract.ModalityText },
		"sampling": func(r *contract.Request, _ *provider.Route) {
			temperature := 0.2
			r.Sampling.Temperature = &temperature
		},
		"speech": func(r *contract.Request, _ *provider.Route) {
			r.Speech = &contract.SpeechParameters{Voice: "alloy", ResponseFormat: "mp3"}
		},
		"remote audio": func(r *contract.Request, _ *provider.Route) {
			url := "https://example.com/a.wav"
			r.Input.Messages[0].Content[0].Source.URL = &url
		},
		"two parts": func(r *contract.Request, _ *provider.Route) {
			m := &r.Input.Messages[0]
			m.Content = append(m.Content, m.Content[0])
		},
		"assistant role": func(r *contract.Request, _ *provider.Route) { r.Input.Messages[0].Role = contract.RoleAssistant },
		"bad base64": func(r *contract.Request, _ *provider.Route) {
			bad := "not base64!"
			r.Input.Messages[0].Content[0].Source.Data = &bad
		},
		"other provider": func(_ *contract.Request, route *provider.Route) { route.Provider = "openai" },
		"no model":       func(_ *contract.Request, route *provider.Route) { route.UpstreamModelID = "" },
	} {
		r, route := transcriptionRequest("gpt-transcribe", "audio/wav", []byte("synthetic"))
		mutate(r, &route)
		if _, err := a.Translate(r, route); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRefusalsAreClassifiedByOpenAIsErrorTypeAndRedacted(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   contract.ErrorCode
		cat    contract.UpstreamErrorCategory
	}{
		{429, `{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"quota"}}`, contract.CodeProviderBillingRefused, contract.UpstreamQuota},
		{429, `{"error":{"type":"requests","code":"rate_limit_exceeded","message":"slow down"}}`, contract.CodeRateLimited, contract.UpstreamRateLimit},
		{402, `{}`, contract.CodeProviderBillingRefused, contract.UpstreamQuota},
		{401, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided: ` + fakeKey + `"}}`, contract.CodeProviderCredentialInvalid, contract.UpstreamAuthentication},
		{404, `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"no"}}`, contract.CodeModelNotFound, contract.UpstreamInvalidReq},
		{413, `{}`, contract.CodeRequestTooLarge, contract.UpstreamInvalidReq},
		{400, `{"error":{"type":"invalid_request_error","message":"Audio file might be corrupted"}}`, contract.CodeInvalidRequest, contract.UpstreamInvalidReq},
		{503, `{}`, contract.CodeProviderOverloaded, contract.UpstreamOverloaded},
		{500, `{}`, contract.CodeProviderError, contract.UpstreamServerError},
	}
	for _, c := range cases {
		a := adapter(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		})
		r, route := transcriptionRequest("gpt-transcribe", "audio/wav", []byte("synthetic"))
		call, _ := a.Translate(r, route)
		outcome, err := a.Stream(context.Background(), call, &capture{}, nil)
		var upstream provider.ErrUpstream
		if !errors.As(err, &upstream) || upstream.Code != c.code || upstream.Category != c.cat {
			t.Errorf("%d %s: %v", c.status, c.body, err)
			continue
		}
		if outcome.KeyID == "" {
			t.Errorf("%d: the refused attempt names no key", c.status)
		}
		if upstream.Passthrough != nil && upstream.Passthrough.Message != nil && strings.Contains(*upstream.Passthrough.Message, fakeKey) {
			t.Errorf("%d: the platform key reached a customer-visible message", c.status)
		}
	}
}

func TestCancellationReachesTheUpstream(t *testing.T) {
	arrived := make(chan struct{})
	released := make(chan struct{})
	a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
		// The server watches for a disconnect only once the body is consumed.
		_, _ = io.Copy(io.Discard, r.Body)
		close(arrived)
		<-r.Context().Done()
		close(released)
	})
	r, route := transcriptionRequest("gpt-transcribe", "audio/wav", []byte("synthetic"))
	call, _ := a.Translate(r, route)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-arrived; cancel() }()
	_, err := a.Stream(ctx, call, &capture{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request outlived the cancelled context")
	}
}

func TestHealthNeverSpendsTranscriptionCredit(t *testing.T) {
	a := adapter(t, func(http.ResponseWriter, *http.Request) { t.Error("a health check reached the upstream") })
	if a.Health(context.Background()).Status != provider.HealthDegraded {
		t.Fatal("a configured credential without a canary claimed full health")
	}
	unconfigured, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.Health(context.Background()).Status != provider.HealthUnconfigured {
		t.Fatal("configuration invented a credential")
	}
}
