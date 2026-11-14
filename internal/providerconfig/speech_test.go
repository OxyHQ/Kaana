package providerconfig

import (
	"reflect"
	"slices"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// xAI's speech statement is the route's reviewed one: five voices whose names
// and genders are the reviewed xAI voice table's, MP3 only, 0.7 to 1.5 and
// 15 000 characters.
func TestTheXAISpeechStatementIsTheReviewedRoute(t *testing.T) {
	statement, stated := SpeechCapabilities("xai", ProtocolOpenAICompatible, XAISpeechEndpoint)
	if !stated {
		t.Fatal("xAI's speech endpoint states nothing")
	}
	want := []Voice{
		{ID: "ara", Name: "Ara"}, {ID: "eve", Name: "Eve", Gender: "female"}, {ID: "leo", Name: "Leo"},
		{ID: "rex", Name: "Rex", Gender: "male"}, {ID: "sal", Name: "Sal"},
	}
	if !reflect.DeepEqual(statement.Voices, want) {
		t.Errorf("voices = %+v, want %+v", statement.Voices, want)
	}
	for _, voice := range statement.Voices {
		if !slices.Contains(xAIRealtimeVoices, voice) {
			t.Errorf("voice %+v is not the reviewed xAI voice table's", voice)
		}
	}
	if !reflect.DeepEqual(statement.OutputMediaTypes, []contract.AudioMediaType{"audio/mpeg"}) ||
		statement.Speed == nil || *statement.Speed != (contract.NumberRange{Min: 0.7, Max: 1.5}) ||
		statement.MaxInputCharacters != 15000 || statement.Languages == nil || len(statement.Languages) != 0 {
		t.Errorf("statement = %+v", statement)
	}
	for alias, voice := range map[string]string{"female": "eve", "male": "rex"} {
		if got, ok := statement.VoiceFor(alias); !ok || got != voice {
			t.Errorf("alias %s = %q, %v; want %s", alias, got, ok, voice)
		}
		if slices.ContainsFunc(statement.Voices, func(v Voice) bool { return v.ID == alias }) {
			t.Errorf("alias %s is published as a voice", alias)
		}
	}
	// A caller's copy is its own.
	statement.Aliases["neutral"] = "ara"
	statement.Voices[0].Name = "Changed"
	again, _ := SpeechCapabilities("xai", ProtocolOpenAICompatible, XAISpeechEndpoint)
	if _, leaked := again.Aliases["neutral"]; leaked || again.Voices[0].Name != "Ara" || xAIRealtimeVoices[1].Name != "Ara" {
		t.Error("a caller changed the reviewed statement")
	}
}

func TestSpeechIsStatedOnlyForASpeechDeployment(t *testing.T) {
	for _, tc := range []struct {
		slug     contract.ProviderSlug
		protocol string
		upstream string
	}{
		{"xai", ProtocolOpenAICompatible, "grok-4.6"},
		{"xai", ProtocolOpenAICompatible, "grok-voice-transcribe-2.0"},
		{"groq", ProtocolOpenAICompatible, XAISpeechEndpoint},
		{"openrouter", ProtocolOpenAICompatible, XAISpeechEndpoint},
		{"xai-realtime", ProtocolXAIRealtime, XAISpeechEndpoint},
	} {
		if _, stated := SpeechCapabilities(tc.slug, tc.protocol, tc.upstream); stated {
			t.Errorf("%s/%s %s states speech", tc.slug, tc.protocol, tc.upstream)
		}
	}
}

// Refusal is what the adapter refuses, field by field, in its order; a
// request inside the statement passes.
func TestASpeechRequestOutsideTheStatementIsRefused(t *testing.T) {
	statement, _ := SpeechCapabilities("xai", ProtocolOpenAICompatible, XAISpeechEndpoint)
	speed := func(value float64) *float64 { return &value }
	for _, tc := range []struct {
		name       string
		characters int
		parameters contract.SpeechParameters
		param      string
	}{
		{"empty", 0, contract.SpeechParameters{Voice: "eve", ResponseFormat: "mp3"}, "input"},
		{"too long", 15001, contract.SpeechParameters{Voice: "eve", ResponseFormat: "mp3"}, "input"},
		{"wav", 10, contract.SpeechParameters{Voice: "eve", ResponseFormat: "wav"}, "speech.responseFormat"},
		{"slow", 10, contract.SpeechParameters{Voice: "eve", ResponseFormat: "mp3", Speed: speed(0.69)}, "speech.speed"},
		{"fast", 10, contract.SpeechParameters{Voice: "eve", ResponseFormat: "mp3", Speed: speed(1.51)}, "speech.speed"},
		// altair is one of xAI's own voices, but not one this route takes.
		{"unstated voice", 10, contract.SpeechParameters{Voice: "altair", ResponseFormat: "mp3"}, "speech.voice"},
		{"unknown voice", 10, contract.SpeechParameters{Voice: "nobody", ResponseFormat: "mp3"}, "speech.voice"},
	} {
		if param, _, refused := statement.Refusal(tc.characters, tc.parameters); !refused || param != tc.param {
			t.Errorf("%s: refused %v on %q, want %q", tc.name, refused, param, tc.param)
		}
	}
	for _, voice := range []string{"ara", "eve", "leo", "rex", "sal", "female", "male"} {
		if param, detail, refused := statement.Refusal(15000, contract.SpeechParameters{Voice: voice, ResponseFormat: "mp3", Speed: speed(1.5)}); refused {
			t.Errorf("voice %s refused on %s: %s", voice, param, detail)
		}
	}
}
