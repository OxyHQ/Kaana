package publisher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// The realtime capabilities and voices a session model is published with
// (contract set 4.1.0; Alia ADR 0017 §5): the adapter's reviewed statement,
// written into the inventory by the publisher and read back by the
// catalogue — never inferred from a model list, which says nothing about
// sessions.

var allRealtimeFormats = []contract.RealtimeAudioFormat{contract.RealtimeALaw, contract.RealtimeULaw, contract.RealtimePCM16}

// TestThePublisherPublishesWhatAnXAIVoiceSessionServes publishes the xAI
// voice model through the real snapshot path and reads the catalogue back
// from the published bytes: transcription declared, the three encodings,
// xAI's documented voices and languages, and a gender only where reviewed.
func TestThePublisherPublishesWhatAnXAIVoiceSessionServes(t *testing.T) {
	fake := newFakeXAIVoice(t, sessionCreated(xaiVoiceModel), conversationCreated)
	table, err := ParseAttribution([]byte(`{"attribution":{"xai-realtime":{"grok-voice-3.0":"x-ai/grok-voice-3.0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{}
	inventoryPublisher, err := New(Config{
		Providers: []Provider{fake.provider()}, Attribution: table, Store: store,
		Client: fake.server.Client(), Logger: quietLogger(),
		Now: func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inventoryPublisher.PublishOnce(context.Background()); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	parsed, err := inventory.Parse(store.written()[0], time.Hour)
	if err != nil {
		t.Fatalf("the published snapshot does not parse: %v", err)
	}
	catalogue := parsed.Catalogue()
	if len(catalogue) != 1 || catalogue[0].Model != "x-ai/grok-voice-3.0" {
		t.Fatalf("catalogue = %+v", catalogue)
	}
	entry := catalogue[0]
	wantRealtime := &inventory.RealtimeObservation{
		Transports:              []contract.RealtimeSessionTransport{contract.RealtimeWebSocket},
		SessionKinds:            []contract.RealtimeSessionKind{contract.RealtimeConversation},
		InputAudioTranscription: true,
		AudioFormats:            inventory.RealtimeAudioFormats{Input: allRealtimeFormats, Output: allRealtimeFormats},
	}
	if !reflect.DeepEqual(entry.Realtime, wantRealtime) {
		t.Errorf("realtime = %+v, want %+v", entry.Realtime, wantRealtime)
	}
	if entry.Audio == nil || len(entry.Audio.Voices) != 26 || len(entry.Audio.Languages) != 20 {
		t.Fatalf("audio = %+v", entry.Audio)
	}
	genders := map[string]string{}
	for _, voice := range entry.Audio.Voices {
		if voice.Gender != "" {
			genders[voice.ID] = voice.Gender
		}
		if !reflect.DeepEqual(voice.Languages, entry.Audio.Languages) {
			t.Errorf("voice %s speaks %v, not every documented language", voice.ID, voice.Languages)
		}
	}
	if want := map[string]string{"eve": "female", "rex": "male"}; !reflect.DeepEqual(genders, want) {
		t.Errorf("genders = %v; only the reviewed product voices carry one: %v", genders, want)
	}
}

// TestOpenAIRealtimeModelsArePublishedWithTheirVoicesAndNoInventedLanguage:
// OpenAI documents its Realtime voices but neither their gender nor the
// languages its models converse in, so the line is published with its ten
// voices, no gender and an empty language list. The text model OpenAI's list
// also returns under the slug is dropped, as before.
func TestOpenAIRealtimeModelsArePublishedWithTheirVoicesAndNoInventedLanguage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected discovery endpoint %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-realtime-2.1","created":1790000000},{"id":"gpt-6-sol"}]}`))
	}))
	t.Cleanup(server.Close)
	target := Provider{Slug: "openai-realtime", BaseURL: server.URL + "/v1", APIKey: "test-key", Protocol: providerconfig.ProtocolOpenAIRealtime}
	models, err := Discover(context.Background(), server.Client(), target)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var realtime *inventory.Observed
	for _, model := range models {
		if model.UpstreamModelID == "gpt-realtime-2.1" {
			realtime = model.Observed
		}
		if model.UpstreamModelID == "gpt-6-sol" && model.Observed != nil && model.Observed.Realtime != nil {
			t.Errorf("a text model was stated as a session model: %+v", model.Observed)
		}
	}
	if realtime == nil || realtime.Realtime == nil || realtime.Audio == nil {
		t.Fatalf("gpt-realtime-2.1 observed = %+v", realtime)
	}
	if realtime.CreatedAt == nil {
		t.Error("the statement displaced what the model list said")
	}
	if err := realtime.Validate(); err != nil {
		t.Fatalf("the published observation does not validate: %v", err)
	}
	if !realtime.Realtime.InputAudioTranscription || len(realtime.Audio.Voices) != 10 || len(realtime.Audio.Languages) != 0 || realtime.Audio.Languages == nil {
		t.Errorf("audio = %+v realtime = %+v", realtime.Audio, realtime.Realtime)
	}
	for _, voice := range realtime.Audio.Voices {
		if voice.Gender != "" || len(voice.Languages) != 0 {
			t.Errorf("voice %+v carries what OpenAI does not document", voice)
		}
	}
}

// TestARequestSlugPublishesNoRealtimeStatement is the negative control: the
// same model list under a request adapter's slug states nothing.
func TestARequestSlugPublishesNoRealtimeStatement(t *testing.T) {
	models := []DiscoveredModel{{UpstreamModelID: "gpt-realtime-2.1"}, {UpstreamModelID: "gpt-6-sol"}}
	applyStatedRealtime(Provider{Slug: "openai", Protocol: providerconfig.Known["openai"].Protocol}, models)
	for _, model := range models {
		if model.Observed != nil {
			t.Errorf("%s was stated under openai: %+v", model.UpstreamModelID, model.Observed)
		}
	}
}
