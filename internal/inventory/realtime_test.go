package inventory_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

const realtimeStatement = `"realtime":{"transports":["websocket"],"sessionKinds":["conversation"],"inputAudioTranscription":true,
  "audioFormats":{"input":["g711_alaw","g711_ulaw","pcm16_24khz"],"output":["g711_alaw","g711_ulaw","pcm16_24khz"]}}`

const audioStatement = `"audio":{"languages":["en","es-ES"],"voices":[
  {"id":"eve","name":"Eve","languages":["en","es-ES"],"gender":"female"},
  {"id":"rex","name":"Rex","languages":["en","es-ES"],"gender":"male"}]}`

// TestParseRefusesARealtimeStatementTheCatalogueCouldNotPublish holds the
// inventory to what the contract's capabilities.realtime and audio refuse,
// and to the sorted, once-only form the publisher writes.
func TestParseRefusesARealtimeStatementTheCatalogueCouldNotPublish(t *testing.T) {
	now := time.Now()
	for name, observed := range map[string]string{
		"no transport":                            `{"realtime":{"transports":[],"sessionKinds":["conversation"],"inputAudioTranscription":true,"audioFormats":{"input":["pcm16_24khz"],"output":[]}}}`,
		"a transport nobody serves":               `{"realtime":{"transports":["webrtc"],"sessionKinds":["conversation"],"inputAudioTranscription":true,"audioFormats":{"input":["pcm16_24khz"],"output":[]}}}`,
		"an unknown session kind":                 `{"realtime":{"transports":["websocket"],"sessionKinds":["dictation"],"inputAudioTranscription":true,"audioFormats":{"input":["pcm16_24khz"],"output":[]}}}`,
		"no input encoding":                       `{"realtime":{"transports":["websocket"],"sessionKinds":["conversation"],"inputAudioTranscription":true,"audioFormats":{"input":[],"output":[]}}}`,
		"a file media type":                       `{"realtime":{"transports":["websocket"],"sessionKinds":["conversation"],"inputAudioTranscription":true,"audioFormats":{"input":["audio/pcm"],"output":[]}}}`,
		"an absent output list":                   `{"realtime":{"transports":["websocket"],"sessionKinds":["conversation"],"inputAudioTranscription":true,"audioFormats":{"input":["pcm16_24khz"]}}}`,
		"an unsorted encoding list":               `{"realtime":{"transports":["websocket"],"sessionKinds":["conversation"],"inputAudioTranscription":true,"audioFormats":{"input":["pcm16_24khz","g711_alaw"],"output":[]}}}`,
		"a language that is not a tag":            `{"audio":{"languages":["Spanish"],"voices":[]}}`,
		"a repeated language":                     `{"audio":{"languages":["en","en"],"voices":[]}}`,
		"a voice speaking an undeclared language": `{"audio":{"languages":["en"],"voices":[{"id":"eve","name":"Eve","languages":["fr"]}]}}`,
		"a repeated voice":                        `{"audio":{"languages":[],"voices":[{"id":"eve","name":"Eve","languages":[]},{"id":"eve","name":"Eve","languages":[]}]}}`,
		"unsorted voices":                         `{"audio":{"languages":[],"voices":[{"id":"rex","name":"Rex","languages":[]},{"id":"eve","name":"Eve","languages":[]}]}}`,
		"an untrimmed voice name":                 `{"audio":{"languages":[],"voices":[{"id":"eve","name":" Eve","languages":[]}]}}`,
		"a gender outside the vocab":              `{"audio":{"languages":[],"voices":[{"id":"eve","name":"Eve","languages":[],"gender":"woman"}]}}`,
	} {
		if _, err := inventory.Parse(issued(now, deploymentWith("d", "xai-realtime", observed)), time.Hour); err == nil || !strings.Contains(err.Error(), "observed") {
			t.Errorf("%s: accepted or refused for another reason: %v", name, err)
		}
	}
	// Control: the statement the publisher writes is accepted.
	valid := `{` + realtimeStatement + `,` + audioStatement + `}`
	if _, err := inventory.Parse(issued(now, deploymentWith("d", "xai-realtime", valid)), time.Hour); err != nil {
		t.Fatalf("control: a stated realtime deployment was refused: %v", err)
	}
}

// TestTheCatalogueNarrowsARealtimeLineToWhatEveryRouteServes: two routes of
// one line, one transcribing with two voices and two languages, the other
// not transcribing, taking only PCM, offering one of the voices with no
// reviewed gender and one language; a third says nothing and abstains. The
// line publishes the intersection, so a session that passes it is servable
// on either route.
func TestTheCatalogueNarrowsARealtimeLineToWhatEveryRouteServes(t *testing.T) {
	narrow := `{"realtime":{"transports":["websocket"],"sessionKinds":["conversation"],"inputAudioTranscription":false,
	  "audioFormats":{"input":["pcm16_24khz"],"output":["pcm16_24khz"]}},
	  "audio":{"languages":["en"],"voices":[{"id":"eve","name":"Eve (B)","languages":["en"]}]}}`
	document := issued(time.Now(), strings.Join([]string{
		deploymentWith("dep_a", "xai-realtime", `{`+realtimeStatement+`,`+audioStatement+`}`),
		deploymentWith("dep_b", "other-realtime", narrow),
		`{"deploymentId":"dep_c","provider":"groq","modelReference":"openai/gpt-oss-120b@observed-2026-08-01","upstreamModelId":"x","current":true}`,
	}, ","))
	entry := parse(t, document).Catalogue()[0]
	wantRealtime := &inventory.RealtimeObservation{
		Transports:   []contract.RealtimeSessionTransport{contract.RealtimeWebSocket},
		SessionKinds: []contract.RealtimeSessionKind{contract.RealtimeConversation},
		AudioFormats: inventory.RealtimeAudioFormats{Input: []contract.RealtimeAudioFormat{contract.RealtimePCM16}, Output: []contract.RealtimeAudioFormat{contract.RealtimePCM16}},
	}
	if !reflect.DeepEqual(entry.Realtime, wantRealtime) {
		t.Errorf("realtime = %+v, want %+v", entry.Realtime, wantRealtime)
	}
	wantAudio := &inventory.AudioObservation{Languages: []string{"en"}, Voices: []inventory.VoiceObservation{{ID: "eve", Name: "Eve", Languages: []string{"en"}}}}
	if !reflect.DeepEqual(entry.Audio, wantAudio) {
		t.Errorf("audio = %+v, want %+v", entry.Audio, wantAudio)
	}

	// Control: the transcribing route alone publishes its whole statement.
	alone := parse(t, issued(time.Now(), deploymentWith("dep_a", "xai-realtime", `{`+realtimeStatement+`,`+audioStatement+`}`))).Catalogue()[0]
	if alone.Realtime == nil || !alone.Realtime.InputAudioTranscription || len(alone.Audio.Voices) != 2 || alone.Audio.Voices[1].Gender != "male" {
		t.Errorf("a single route = %+v %+v", alone.Realtime, alone.Audio)
	}
}
