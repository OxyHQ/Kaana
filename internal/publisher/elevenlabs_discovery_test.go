package publisher_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

// testdata/elevenlabs/models.json is shaped field for field from ElevenLabs'
// documented `GET /v1/models` response (ModelResponseModel,
// https://elevenlabs.io/docs/api-reference/models/list): a bare JSON array.
// Its values are illustrative, not a capture: the production key only answers
// from Kaana's NAT egress, so the first publisher cycle in production is the
// first real read, and its dropped-id warnings are the evidence any later
// attribution change starts from.
func elevenLabsModelListServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s?%s, want GET /v1/models", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("xi-api-key") != "test-key" {
			t.Errorf("xi-api-key = %q", r.Header.Get("xi-api-key"))
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Errorf("ElevenLabs was sent an Authorization header: %q", authorization)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func elevenLabsProvider(base string) publisher.Provider {
	return publisher.Provider{
		Slug: "elevenlabs", BaseURL: base, APIKey: "test-key",
		Discovery: providerconfig.DiscoveryElevenLabsModels, Protocol: providerconfig.ProtocolElevenLabsSpeech,
	}
}

func TestElevenLabsDiscoveryKeepsOnlyTextToSpeechRows(t *testing.T) {
	fixture, err := os.ReadFile("testdata/elevenlabs/models.json")
	if err != nil {
		t.Fatal(err)
	}
	server := elevenLabsModelListServer(t, http.StatusOK, string(fixture))
	models, err := publisher.Discover(context.Background(), server.Client(), elevenLabsProvider(server.URL+"/v1"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var ids []string
	for _, model := range models {
		ids = append(ids, model.UpstreamModelID)
		observed := model.Observed
		if observed == nil || !reflect.DeepEqual(observed.InputModalities, []string{"text"}) || !reflect.DeepEqual(observed.OutputModalities, []string{"audio"}) {
			t.Errorf("%s observed = %+v, want text in and audio out", model.UpstreamModelID, observed)
		}
		if observed != nil && (observed.ListPrice != nil || observed.ContextTokens != nil || observed.AcceptedParameters != nil || observed.SupportsTools != nil) {
			t.Errorf("%s observed something ElevenLabs' list does not state: %+v", model.UpstreamModelID, observed)
		}
	}
	// The voice changer (can_do_text_to_speech false) and the alpha row are
	// never candidates; the deprecated Turbo row is a TTS row, so discovery
	// keeps it and attribution drops it.
	if want := []string{"eleven_flash_v2_5", "eleven_multilingual_v2", "eleven_turbo_v2_5"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("models = %v, want %v", ids, want)
	}
	if name := models[1].Observed.DisplayName; name == nil || *name != "Eleven Multilingual v2" {
		t.Errorf("display name = %v", name)
	}
}

func TestElevenLabsDiscoveryRefusesWhatItCannotRead(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"invalid key (measured 2026-10-12)": {http.StatusUnauthorized, `{"detail":{"type":"authentication_error","code":"unauthorized","message":"Invalid API key","status":"invalid_api_key","request_id":"r"}}`, "answered 401"},
		"key not allowed from this IP":      {http.StatusForbidden, `{"detail":{"type":"authorization_error","code":"forbidden","message":"IP not allowed","request_id":"r"}}`, "answered 403"},
		"an OpenAI-shaped list":             {http.StatusOK, `{"data":[{"id":"eleven_multilingual_v2"}]}`, "not the documented ElevenLabs list shape"},
		"a duplicate id":                    {http.StatusOK, `[{"model_id":"eleven_v3","can_do_text_to_speech":true},{"model_id":"eleven_v3","can_do_text_to_speech":true}]`, "twice"},
		"a blank id":                        {http.StatusOK, `[{"model_id":" ","can_do_text_to_speech":true}]`, "missing or whitespace-normalized"},
		"a flag that is not a boolean":      {http.StatusOK, `[{"model_id":"eleven_v3","can_do_text_to_speech":"yes"}]`, "not the documented ElevenLabs list shape"},
		"no speech model at all":            {http.StatusOK, `[{"model_id":"eleven_english_sts_v2","can_do_text_to_speech":false}]`, "no text-to-speech models"},
	} {
		t.Run(name, func(t *testing.T) {
			server := elevenLabsModelListServer(t, c.status, c.body)
			_, err := publisher.Discover(context.Background(), server.Client(), elevenLabsProvider(server.URL+"/v1"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
			if strings.Contains(err.Error(), "test-key") {
				t.Fatal("the discovery credential reached an error")
			}
		})
	}
}

// TestTheElevenLabsListIsReadOnlyForTheElevenLabsSlug: the profile
// authenticates with the speech adapter's header, which no other slug serves.
func TestTheElevenLabsListIsReadOnlyForTheElevenLabsSlug(t *testing.T) {
	target := elevenLabsProvider("https://api.elevenlabs.io/v1")
	target.Slug = "openai"
	if _, err := publisher.Discover(context.Background(), http.DefaultClient, target); err == nil {
		t.Fatal("another slug read ElevenLabs' model list")
	}
}
