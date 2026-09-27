package deepgram

import (
	"bytes"
	"context"
	"encoding/base64"
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

const fakeKey = "deepgram-synthetic-credential-not-valid"

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func adapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a, err := New(Config{BaseURL: "https://api.deepgram.com/v1", Declarations: provider.DeclareKeys([]string{fakeKey}), HTTPClient: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.deepgram.com" || r.Header.Get("Authorization") != "Token "+fakeKey {
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
	audio   []byte
	units   []contract.UsageQuantity
	fail    error
}

func (c *capture) Start(contract.ModelReference, time.Time) error { c.started = true; return nil }
func (c *capture) Delta(_ int, _ contract.DeltaChannel, text string) error {
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
func (c *capture) Audio(i int, mt string, data []byte) error {
	if !c.started || i != 0 || mt != "audio/mpeg" || len(data) > 49152 {
		return errors.New("invalid audio event")
	}
	c.audio = append(c.audio, data...)
	return c.fail
}
func speech() (*contract.Request, provider.Route) {
	text := "Hola mundo"
	return &contract.Request{Modality: contract.ModalityAudio, Input: contract.Input{Format: contract.InputText, Text: &text}, Client: contract.ClientRequestMetadata{APIFormat: contract.APIFormatAudioSpeech}, Speech: &contract.SpeechParameters{Voice: "aura-2-thalia-en", ResponseFormat: "mp3"}}, provider.Route{Provider: Slug, UpstreamModelID: "aura-2-thalia-en", ModelReference: "deepgram/aura-2-thalia-en@2026-09-27"}
}
func transcription() (*contract.Request, provider.Route) {
	r, route := speech()
	r.Speech = nil
	r.Client.APIFormat = contract.APIFormatAudioTranscriptions
	data, mt := base64.StdEncoding.EncodeToString([]byte("RIFFtest-wave")), "audio/wav"
	r.Input = contract.Input{Format: contract.InputMessages, Messages: []contract.Message{{Role: contract.RoleUser, Content: []contract.ContentPart{{Type: contract.ContentPartAudio, Source: &contract.ContentSource{Kind: contract.ContentSourceInline, Data: &data, MediaType: &mt}}}}}}
	route.UpstreamModelID = "nova-3"
	route.ModelReference = "deepgram/nova-3@2026-09-27"
	return r, route
}
func TestSpeechWireAndMeasuredUsageSurviveSinkFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "sink failure"}[fail], func(t *testing.T) {
			calls := 0
			audio := append([]byte("ID3\x04"), bytes.Repeat([]byte{0}, 60000)...)
			a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/v1/speak" || r.URL.Query().Get("model") != "aura-2-thalia-en" || r.URL.Query().Get("encoding") != "mp3" {
					t.Error("wrong speech request")
				}
				var b map[string]string
				if json.NewDecoder(r.Body).Decode(&b) != nil || len(b) != 1 || b["text"] != "Hola mundo" {
					t.Error("wrong speech body")
				}
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Header().Set("dg-char-count", "10")
				_, _ = w.Write(audio)
			})
			r, route := speech()
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
			if calls != 1 || len(out.Units) != 1 || out.Units[0].Unit != contract.UnitCharacters || out.Units[0].Quantity != 10 || out.UsageSource != contract.UsageProviderReported {
				t.Fatalf("wrong metering: %+v", out)
			}
			if !fail && !bytes.Equal(c.audio, audio) {
				t.Fatal("audio changed")
			}
		})
	}
}
func TestTranscriptionWireAndDuration(t *testing.T) {
	a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/listen" || r.URL.Query().Get("model") != "nova-3" || r.Header.Get("Content-Type") != "audio/wav" || string(b) != "RIFFtest-wave" {
			t.Error("wrong transcription wire")
		}
		_, _ = io.WriteString(w, `{"metadata":{"duration":1.2345,"channels":1},"results":{"channels":[{"alternatives":[{"transcript":"Hola"}]}]}}`)
	})
	r, route := transcription()
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	c := &capture{}
	out, err := a.Stream(context.Background(), call, c, nil)
	if err != nil || c.text != "Hola" || len(out.Units) != 1 || out.Units[0].Unit != contract.UnitAudioInputMilliseconds || out.Units[0].Quantity != 1235 {
		t.Fatalf("result %+v, %v", out, err)
	}
}
func TestUnsupportedControlsSpendNothing(t *testing.T) {
	calls := 0
	a := adapter(t, func(http.ResponseWriter, *http.Request) { calls++ })
	cases := map[string]func(*contract.Request){"stream": func(r *contract.Request) { r.Stream = true }, "voice": func(r *contract.Request) { r.Speech.Voice = "female" }, "format": func(r *contract.Request) { r.Speech.ResponseFormat = "wav" }, "speed": func(r *contract.Request) { x := 1.0; r.Speech.Speed = &x }, "sampling": func(r *contract.Request) { x := 0.5; r.Sampling.Temperature = &x }, "length": func(r *contract.Request) { s := strings.Repeat("x", 2001); r.Input.Text = &s }, "chat": func(r *contract.Request) { r.Client.APIFormat = contract.APIFormatChatCompletions }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r, route := speech()
			change(r)
			_, err := a.Translate(r, route)
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Param == "" {
				t.Fatalf("missing refusal: %v", err)
			}
		})
	}
	if calls != 0 {
		t.Fatal("unsupported requests reached provider")
	}
}
func TestErrorsAreClassifiedAndCredentialsRedacted(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   contract.ErrorCode
	}{{402, "ASR_PAYMENT_REQUIRED", contract.CodeProviderBillingRefused}, {401, "INVALID_AUTH", contract.CodeProviderCredentialInvalid}, {403, "INSUFFICIENT_PERMISSIONS", contract.CodeProviderCredentialInvalid}, {429, "RATE_LIMIT", contract.CodeRateLimited}, {500, "UNKNOWN", contract.CodeProviderError}, {400, "Bad Request", contract.CodeInvalidRequest}} {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"err_code": tc.code, "err_msg": "diagnostic echoes " + fakeKey})
			})
			r, route := speech()
			call, err := a.Translate(r, route)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Stream(context.Background(), call, &capture{}, nil)
			var upstream provider.ErrUpstream
			if !errors.As(err, &upstream) || upstream.Code != tc.want {
				t.Fatalf("wrong refusal: %v", err)
			}
			if calls != 1 || upstream.Passthrough == nil || upstream.Passthrough.Message == nil || !strings.Contains(*upstream.Passthrough.Message, "diagnostic") || strings.Contains(*upstream.Passthrough.Message, fakeKey) {
				t.Fatal("redaction lost diagnostic or leaked credential")
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
	r, route := speech()
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
func TestInvalidAudioRetainsReportedUsage(t *testing.T) {
	a := adapter(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("dg-char-count", "10")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "not audio")
	})
	r, route := speech()
	call, err := a.Translate(r, route)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Stream(context.Background(), call, &capture{}, nil)
	if err == nil || len(out.Units) != 1 || out.Units[0].Quantity != 10 {
		t.Fatalf("lost measured units: %+v %v", out, err)
	}
}
func TestEndpointAndUnconfiguredHealth(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://example.com/v1"}); err == nil {
		t.Fatal("untrusted origin accepted")
	}
	a, err := New(Config{BaseURL: "https://api.deepgram.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Health(context.Background()).Status != provider.HealthUnconfigured {
		t.Fatal("missing credential reported healthy")
	}
}
