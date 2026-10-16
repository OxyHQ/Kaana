package publicapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"

	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// compatFixturesPath is what this build sends its readers, recorded for
// packages/sdk/src/__tests__/compatibility.test.ts, which parses it with the
// exact @kaana.ai/contracts releases running in production: a change that
// would break a deployed client fails there before it merges.
const compatFixturesPath = "../../packages/sdk/src/__tests__/compat/kaana-sends.generated.json"

// TestTheCompatibilityFixturesAreWhatKaanaSends records, end to end through the
// public API, what each deployed reader receives: the catalogue and a
// transcription (a client), every public frame of a realtime session (a
// client), and the authorizations and settlements (Oxy). The recording is
// compared with volatile values (ids, times, measured durations) masked; a
// change fails until it is regenerated with KAANA_UPDATE_COMPAT_FIXTURES=1.
func TestTheCompatibilityFixturesAreWhatKaanaSends(t *testing.T) {
	h := newHarness(t, harnessOptions{catalogue: loadDeclared(t)})
	spoken := base64.StdEncoding.EncodeToString(make([]byte, 4800))
	h.upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "input_audio_buffer.committed", "item_id": "item_user"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.delta", "item_id": "item_user", "content_index": 0, "delta": "hol"})
		conn.Send(fake.Event{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "content_index": 0, "transcript": "hola", "usage": fake.TranscriptionDuration(0.5)})
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "object": "realtime.response", "status": "in_progress", "output": []any{}}})
		conn.Send(fake.Event{"type": "response.output_audio_transcript.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0, "delta": "Hi"})
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0, "delta": spoken})
		conn.Send(fake.Event{"type": "response.output_audio.done", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0})
		conn.Send(fake.Event{"type": "response.output_audio_transcript.done", "response_id": "resp_1", "item_id": "item_answer", "content_index": 0, "transcript": "Hi"})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "output": []any{}, "usage": fake.Usage(10, 20, 0, 0, 3, 40)}})
		conn.WaitForClose()
	}
	// Alia's call: spoken and written, transcribed, with a tool.
	ws := h.dialRealtime(validKey, conversationOpen(map[string]any{
		"outputModalities": []string{"text", "audio"}, "voice": "marin", "outputAudioFormat": "pcm16_24khz",
		"inputAudioTranscription": map[string]any{"language": "es-AR"},
	}))
	var frames []json.RawMessage
	record := func(events []event) {
		for _, read := range events {
			frames = append(frames, json.RawMessage(read.raw))
		}
	}
	record(readUntil(t, ws, "response.done"))
	var created struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(frames[0], &created)
	// A command the provider cannot take: acknowledged, then a non-fatal error.
	sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": created.RequestID, "commandId": "cmd_refused", "type": "conversation.item.create",
		"item": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_audio", "format": "pcm16_24khz"}}}})
	record(readUntil(t, ws, "error"))
	sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": created.RequestID, "commandId": "cmd_close", "type": "session.close"})
	record(readUntil(t, ws, "session.closed"))

	transcription := h.post("/v1/audio/transcriptions", validKey, map[string]any{
		"model": "stub/whisper", "language": "es-AR", "prompt": "Kaana, Alia",
		"audio": map[string]any{"mediaType": "audio/wav", "data": base64.StdEncoding.EncodeToString(wav(1500))},
	})
	if transcription.StatusCode != http.StatusOK {
		t.Fatalf("transcription: %d %s", transcription.StatusCode, transcription.Body)
	}
	models := h.get("/v1/models", validKey)
	if models.StatusCode != http.StatusOK {
		t.Fatalf("models: %d %s", models.StatusCode, models.Body)
	}
	h.settle()

	h.oxy.mu.Lock()
	settlements := append([]json.RawMessage{}, h.oxy.settlementBodies...)
	authorizations := append([]json.RawMessage{}, h.oxy.authorizations...)
	h.oxy.mu.Unlock()
	// The session's and the transcription's grants, and the catalogue read.
	if len(authorizations) != 3 || len(settlements) != 2 {
		t.Fatalf("%d authorizations and %d settlements, want 3 and 2", len(authorizations), len(settlements))
	}
	recorded := map[string]any{
		"catalogue":      json.RawMessage(models.Body),
		"realtimeEvents": frames,
		"transcription":  json.RawMessage(transcription.Body),
		"authorizations": authorizations,
		"settlements":    settlements,
	}
	encoded, err := json.MarshalIndent(recorded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if os.Getenv("KAANA_UPDATE_COMPAT_FIXTURES") == "1" {
		if err := os.WriteFile(compatFixturesPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(compatFixturesPath)
	if err != nil || !reflect.DeepEqual(masked(t, committed), masked(t, encoded)) {
		t.Fatalf("%s is not what Kaana sends; regenerate it with KAANA_UPDATE_COMPAT_FIXTURES=1 go test ./internal/publicapi -run TestTheCompatibilityFixturesAreWhatKaanaSends", compatFixturesPath)
	}
}

// volatile are the values that differ between two runs of the same exchange.
var volatile = regexp.MustCompile(`^(req|grt|gen|stl|rcpt|att|sess|evt|cost)_[a-z0-9]+$|^\d{4}-\d\d-\d\dT[\d:.]+Z$|^[0-9a-f]{8}-[0-9a-f]{4}-`)

// volatileFields are measured by the clock rather than fixed by the exchange.
var volatileFields = map[string]bool{"latencyMs": true, "durationMs": true, "timeToFirstTokenMs": true, "quantity": true}

func masked(t *testing.T, document []byte) any {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	var mask func(key string, value any) any
	mask = func(key string, value any) any {
		switch typed := value.(type) {
		case map[string]any:
			for field, inner := range typed {
				typed[field] = mask(field, inner)
			}
		case []any:
			for index, inner := range typed {
				typed[index] = mask(key, inner)
			}
		case string:
			if volatile.MatchString(typed) {
				return "<volatile>"
			}
		case json.Number:
			if volatileFields[key] {
				return "<measured>"
			}
		}
		return value
	}
	masked := mask("", value).(map[string]any)
	// Oxy receives the grants and settlements of concurrent work in no fixed
	// order.
	for _, unordered := range []string{"authorizations", "settlements"} {
		items, _ := masked[unordered].([]any)
		sort.Slice(items, func(i, j int) bool {
			left, _ := json.Marshal(items[i])
			right, _ := json.Marshal(items[j])
			return string(left) < string(right)
		})
	}
	return masked
}
