package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const fakeKey = "sk_elevenlabs_synthetic_credential_not_valid"

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// adapter points the adapter at a fake that speaks ElevenLabs' real wire: the
// request still names api.elevenlabs.io and carries `xi-api-key`, and is then
// redirected to the local server.
func adapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a, err := New(Config{BaseURL: "https://api.elevenlabs.io/v1", Declarations: provider.DeclareKeys([]string{fakeKey}), HTTPClient: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.elevenlabs.io" || r.Header.Get("xi-api-key") != fakeKey || r.Header.Get("Authorization") != "" {
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
	audio   []byte
	units   []contract.UsageQuantity
	source  contract.UsageSource
	fail    error
}

func (c *capture) Start(contract.ModelReference, time.Time) error { c.started = true; return nil }
func (c *capture) Delta(int, contract.DeltaChannel, string) error {
	return errors.New("unexpected text")
}
func (c *capture) ToolCall(provider.ToolCallDelta) error { return errors.New("unexpected tool") }
func (c *capture) Usage(u []contract.UsageQuantity, s contract.UsageSource) error {
	if !c.started {
		return errors.New("usage before start")
	}
	c.units, c.source = u, s
	return nil
}
func (c *capture) Audio(i int, mt string, data []byte) error {
	if !c.started || i != 0 || mt != "audio/mpeg" || len(data) > 49152 {
		return errors.New("invalid audio event")
	}
	c.audio = append(c.audio, data...)
	return c.fail
}

func speech(voice string) (*contract.Request, provider.Route) {
	text := "Hola, ¿qué tal? 🌍"
	return &contract.Request{
			Modality: contract.ModalityAudio,
			Input:    contract.Input{Format: contract.InputText, Text: &text},
			Client:   contract.ClientRequestMetadata{APIFormat: contract.APIFormatAudioSpeech},
			Speech:   &contract.SpeechParameters{Voice: voice, ResponseFormat: "mp3"},
		}, provider.Route{
			Provider: Slug, UpstreamModelID: "eleven_flash_v2_5",
			ModelReference: "elevenlabs/eleven-flash-v2.5@observed-2026-10-12",
		}
}

// mp3 is an ID3-tagged body larger than one audio event, so chunking is
// exercised.
var mp3 = append([]byte("ID3\x04"), bytes.Repeat([]byte{0}, 60000)...)

func TestSpeechWireAndMeasuredUsageSurviveSinkFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "sink failure"}[fail], func(t *testing.T) {
			calls := 0
			a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				// https://elevenlabs.io/docs/api-reference/text-to-speech/convert
				if r.Method != http.MethodPost || r.URL.Path != "/v1/text-to-speech/EXAVITQu4vr4xnSDxMaL" || r.URL.RawQuery != "output_format=mp3_44100_128" {
					t.Errorf("wrong speech request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				}
				if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "audio/mpeg" {
					t.Error("wrong speech headers")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["text"] != "Hola, ¿qué tal? 🌍" || body["model_id"] != "eleven_flash_v2_5" {
					t.Errorf("wrong speech body %v", body)
				}
				w.Header().Set("Content-Type", "audio/mpeg")
				_, _ = w.Write(mp3)
			})
			r, route := speech("female")
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			c := &capture{}
			if fail {
				c.fail = errors.New("sink closed")
			}
			out, err := a.Stream(context.Background(), call, c, nil)
			if fail && !errors.Is(err, c.fail) || !fail && err != nil {
				t.Fatal(err)
			}
			// 18 UTF-16 code units: 17 characters, the emoji counted twice.
			if calls != 1 || len(out.Units) != 1 || out.Units[0].Unit != contract.UnitCharacters || out.Units[0].Quantity != 18 || out.UsageSource != contract.UsageOxyMeasured {
				t.Fatalf("wrong metering: %+v", out)
			}
			if !fail && (!bytes.Equal(c.audio, mp3) || c.source != contract.UsageOxyMeasured || out.FinishReason != contract.FinishStop) {
				t.Fatal("audio or usage changed")
			}
		})
	}
}

func TestVoicesAndSpeed(t *testing.T) {
	a := adapter(t, func(http.ResponseWriter, *http.Request) {})
	for voice, path := range map[string]string{
		"female":               "/v1/text-to-speech/EXAVITQu4vr4xnSDxMaL",
		"male":                 "/v1/text-to-speech/JBFqnCBsd6RMkjVDRZzb",
		"pNInz6obpgDQGcFmaJgB": "/v1/text-to-speech/pNInz6obpgDQGcFmaJgB",
	} {
		r, route := speech(voice)
		speed := 1.15
		r.Speech.Speed = &speed
		call, err := a.Translate(r, route)
		if err != nil {
			t.Fatalf("%s: %v", voice, err)
		}
		if !strings.HasSuffix(strings.Split(call.URL, "?")[0], path) {
			t.Errorf("%s -> %s, want %s", voice, call.URL, path)
		}
		var body struct {
			VoiceSettings map[string]float64 `json:"voice_settings"`
		}
		if json.Unmarshal(call.Body, &body) != nil || len(body.VoiceSettings) != 1 || body.VoiceSettings["speed"] != 1.15 {
			t.Errorf("%s: voice_settings = %v, want speed alone", voice, body.VoiceSettings)
		}
	}
}

func TestUnsupportedControlsSpendNothing(t *testing.T) {
	calls := 0
	a := adapter(t, func(http.ResponseWriter, *http.Request) { calls++ })
	cases := map[string]func(*contract.Request, *provider.Route){
		"stream":        func(r *contract.Request, _ *provider.Route) { r.Stream = true },
		"unknown voice": func(r *contract.Request, _ *provider.Route) { r.Speech.Voice = "eve" },
		"voice path":    func(r *contract.Request, _ *provider.Route) { r.Speech.Voice = "../../../v1/user/aaaa" },
		"format":        func(r *contract.Request, _ *provider.Route) { r.Speech.ResponseFormat = "wav" },
		"slow":          func(r *contract.Request, _ *provider.Route) { x := 0.5; r.Speech.Speed = &x },
		"fast":          func(r *contract.Request, _ *provider.Route) { x := 1.5; r.Speech.Speed = &x },
		"sampling":      func(r *contract.Request, _ *provider.Route) { x := 0.5; r.Sampling.Temperature = &x },
		"reasoning": func(r *contract.Request, _ *provider.Route) {
			r.Reasoning = &contract.ReasoningParameters{Effort: "low"}
		},
		"empty": func(r *contract.Request, _ *provider.Route) { s := ""; r.Input.Text = &s },
		"too long for v3": func(r *contract.Request, route *provider.Route) {
			s := strings.Repeat("x", 5001)
			r.Input.Text = &s
			route.UpstreamModelID = "eleven_v3"
		},
		"unreviewed model": func(r *contract.Request, route *provider.Route) {
			route.UpstreamModelID = "eleven_turbo_v2_5"
		},
		"chat":           func(r *contract.Request, _ *provider.Route) { r.Client.APIFormat = contract.APIFormatChatCompletions },
		"no speech":      func(r *contract.Request, _ *provider.Route) { r.Speech = nil },
		"other provider": func(_ *contract.Request, route *provider.Route) { route.Provider = "xai" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, route := speech("female")
			change(r, &route)
			_, err := a.Translate(r, route)
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Param == "" {
				t.Fatalf("missing refusal: %v", err)
			}
		})
	}
	// Positive control: the longest text v3 takes is admitted.
	r, route := speech("female")
	s := strings.Repeat("x", 5000)
	r.Input.Text, route.UpstreamModelID = &s, "eleven_v3"
	if _, err := a.Translate(r, route); err != nil {
		t.Fatalf("the documented v3 ceiling was refused: %v", err)
	}
	if calls != 0 {
		t.Fatal("unsupported requests reached provider")
	}
}

// The error bodies are ElevenLabs' documented shape
// (https://elevenlabs.io/docs/developers/resources/errors); the two 401s are
// the bodies api.elevenlabs.io returned on 2026-10-12 to an invalid and a
// missing key, with the request id shortened.
func TestErrorsAreClassifiedAndCredentialsRedacted(t *testing.T) {
	detail := func(kind, code, status string) string {
		body, _ := json.Marshal(map[string]any{"detail": map[string]string{"type": kind, "code": code, "status": status, "message": "diagnostic echoes " + fakeKey, "request_id": "r"}})
		return string(body)
	}
	for name, tc := range map[string]struct {
		status  int
		body    string
		want    contract.ErrorCode
		verdict provider.CredentialVerdict
	}{
		"invalid key":             {401, detail("authentication_error", "unauthorized", "invalid_api_key"), contract.CodeProviderCredentialInvalid, provider.CredentialRejected},
		"missing key":             {401, detail("authentication_error", "unauthorized", "needs_authorization"), contract.CodeProviderCredentialInvalid, provider.CredentialRejected},
		"ip not allowlisted":      {403, detail("authorization_error", "forbidden", ""), contract.CodeProviderCredentialInvalid, provider.CredentialRejected},
		"scope restricted":        {403, detail("authorization_error", "insufficient_permissions", ""), contract.CodeProviderCredentialInvalid, provider.CredentialRejected},
		"undocumented 403":        {403, `{"detail":"forbidden ` + fakeKey + `"}`, contract.CodeProviderCredentialInvalid, provider.CredentialRejected},
		"credits exhausted":       {402, detail("payment_required", "insufficient_credits", ""), contract.CodeProviderBillingRefused, provider.CredentialExhausted},
		"legacy quota exceeded":   {401, detail("", "", "quota_exceeded"), contract.CodeProviderBillingRefused, provider.CredentialExhausted},
		"voice the account lacks": {403, detail("authorization_error", "voice_access_denied", ""), contract.CodePermissionDenied, provider.CredentialRequestFault},
		"model the plan lacks":    {403, detail("authorization_error", "model_access_denied", ""), contract.CodePermissionDenied, provider.CredentialRequestFault},
		"rate limited":            {429, detail("rate_limit_error", "rate_limit_exceeded", ""), contract.CodeRateLimited, provider.CredentialHealthy},
		"concurrency":             {429, detail("rate_limit_error", "concurrent_limit_exceeded", ""), contract.CodeRateLimited, provider.CredentialHealthy},
		"busy":                    {429, detail("rate_limit_error", "system_busy", ""), contract.CodeProviderOverloaded, provider.CredentialHealthy},
		"voice not found":         {404, detail("not_found", "voice_not_found", ""), contract.CodeInvalidRequest, provider.CredentialRequestFault},
		"model not found":         {404, detail("not_found", "model_not_found", ""), contract.CodeModelNotFound, provider.CredentialRequestFault},
		"text too long":           {400, detail("validation_error", "text_too_long", ""), contract.CodeInvalidRequest, provider.CredentialRequestFault},
		"validation list":         {422, `{"detail":[{"loc":["body","text"],"msg":"field required ` + fakeKey + `","type":"missing"}]}`, contract.CodeInvalidRequest, provider.CredentialRequestFault},
		"maintenance":             {503, detail("service_unavailable", "maintenance", ""), contract.CodeProviderOverloaded, provider.CredentialHealthy},
		"internal":                {500, detail("internal_error", "internal_error", ""), contract.CodeProviderError, provider.CredentialHealthy},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			r, route := speech("female")
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.Stream(context.Background(), call, &capture{}, nil)
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || upstream.Code != tc.want {
				t.Fatalf("wrong refusal: %v", err)
			}
			if got := provider.CredentialVerdictFor(err); got != tc.verdict {
				t.Errorf("verdict = %v, want %v", got, tc.verdict)
			}
			if len(out.Units) != 0 {
				t.Errorf("a refused request was metered: %+v", out.Units)
			}
			if calls != 1 || upstream.Passthrough == nil || upstream.Passthrough.Message == nil || strings.Contains(*upstream.Passthrough.Message, fakeKey) {
				t.Fatal("credential leaked or passthrough lost")
			}
			if strings.HasPrefix(tc.body, `{"detail":{`) && !strings.Contains(*upstream.Passthrough.Message, "diagnostic") {
				t.Fatal("redaction lost the diagnostic")
			}
			if tc.want == contract.CodeRateLimited && upstream.RetryAfterMs != 2000 {
				t.Errorf("Retry-After = %d ms, want 2000", upstream.RetryAfterMs)
			}
		})
	}
}

func TestCancellationReachesUpstream(t *testing.T) {
	seen := make(chan struct{})
	canceled := make(chan struct{})
	a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(seen)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	})
	r, route := speech("male")
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.Stream(ctx, call, &capture{}, nil); done <- err }()
	select {
	case <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not return")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not canceled")
	}
}

// TestInvalidAudioRetainsMeasuredUsage: ElevenLabs accepted and billed the
// text even when what came back is not usable audio.
func TestInvalidAudioRetainsMeasuredUsage(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter){
		"not audio": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "not audio")
		},
		"not mp3": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = io.WriteString(w, "RIFF0000WAVE")
		},
		"oversized": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(append([]byte("ID3\x04"), make([]byte, maxAudioBytes)...))
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := adapter(t, func(w http.ResponseWriter, _ *http.Request) { respond(w) })
			r, route := speech("female")
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			c := &capture{}
			out, err := a.Stream(context.Background(), call, c, nil)
			if err == nil || len(out.Units) != 1 || out.Units[0].Quantity != 18 || c.started {
				t.Fatalf("lost measured units or emitted bad audio: %+v %v", out, err)
			}
		})
	}
}

func TestEndpointAndUnconfiguredHealth(t *testing.T) {
	for _, base := range []string{"https://example.com/v1", "https://api.eu.residency.elevenlabs.io/v1", "http://api.elevenlabs.io/v1"} {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Fatalf("unreviewed origin %s accepted", base)
		}
	}
	a, err := New(Config{BaseURL: "https://api.elevenlabs.io/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Health(context.Background()).Status != provider.HealthUnconfigured {
		t.Fatal("missing credential reported healthy")
	}
	if formats := a.APIFormats(); len(formats) != 1 || formats[0] != contract.APIFormatAudioSpeech {
		t.Fatalf("declared formats = %v", formats)
	}
	configured := adapter(t, func(http.ResponseWriter, *http.Request) {})
	if configured.Health(context.Background()).Status != provider.HealthDegraded {
		t.Fatal("a configured key is healthy before any signed canary")
	}
}
