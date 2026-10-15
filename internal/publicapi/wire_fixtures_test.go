package publicapi_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
)

// TestWritePublicWireFixtures captures what the public server actually wrote
// to a client — bodies, stream frames and realtime events, from real requests
// through the harness — and writes each, named by the contract schema it must
// satisfy, for tools/contract/validate.mjs to parse with @kaana.ai/contracts.
// The Go side never decides whether its own output is valid.
//
// The invalid controls are real captured bodies with one leak added (a
// deployment, a provider error): the strict public schemas must refuse each.
func TestWritePublicWireFixtures(t *testing.T) {
	type fixture struct {
		Schema string          `json:"schema"`
		Case   string          `json:"case"`
		Value  json.RawMessage `json:"value"`
	}
	var valid, invalid []fixture
	add := func(schema, name string, body []byte) {
		t.Helper()
		if !json.Valid(body) {
			t.Fatalf("%s/%s is not JSON: %s", schema, name, body)
		}
		valid = append(valid, fixture{Schema: schema, Case: name, Value: append([]byte(nil), body...)})
	}
	leak := func(schema, name string, body []byte, field string, value any) {
		t.Helper()
		var object map[string]any
		if err := json.Unmarshal(body, &object); err != nil {
			t.Fatal(err)
		}
		object[field] = value
		encoded, _ := json.Marshal(object)
		invalid = append(invalid, fixture{Schema: schema, Case: name, Value: encoded})
	}

	h := newHarness(t, harnessOptions{catalogue: loadDeclared(t)})
	// Responses, whole and streamed.
	whole := h.post("/v1/responses", validKey, responsesBody(map[string]any{
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}},
	}))
	add("responseSchema", "with-tool-call", whole.Body)
	leak("responseSchema", "deployment-leak", whole.Body, "deploymentId", "dep_stub_b")
	for index, frame := range readSSE(t, h.post("/v1/responses", validKey, responsesBody(map[string]any{"stream": true}))) {
		add("responseStreamEventSchema", fmt.Sprintf("frame-%02d-%s", index, frame.event), frame.data)
	}
	h.adapter.failAfter = 2
	frames := readSSE(t, h.post("/v1/responses", validKey, responsesBody(map[string]any{"stream": true})))
	add("responseStreamEventSchema", "error-after-output", frames[len(frames)-1].data)
	h.adapter.failAfter = 0

	// Chat completions, whole, streamed, refused.
	chat := map[string]any{"model": "stub/model", "messages": []any{map[string]any{"role": "user", "content": "Hola"}}}
	add("chatCompletionSchema", "whole", h.post("/v1/chat/completions", validKey, chat).Body)
	chat["stream"] = true
	scanner := bufio.NewScanner(bytes.NewReader(h.post("/v1/chat/completions", validKey, chat).Body))
	for index := 0; scanner.Scan(); {
		if data, found := strings.CutPrefix(scanner.Text(), "data: "); found && data != "[DONE]" {
			add("chatCompletionChunkSchema", fmt.Sprintf("chunk-%02d", index), []byte(data))
			index++
		}
	}
	add("chatCompletionErrorSchema", "refused", h.post("/v1/chat/completions", revokedKey, chat).Body)

	// Speech frames, transcription, errors.
	for index, frame := range readSSE(t, h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "stream": true, "stream_format": "sse"})) {
		add("speechStreamEventSchema", fmt.Sprintf("frame-%02d", index), frame.data)
	}
	transcription := h.post("/v1/audio/transcriptions", validKey, map[string]any{"model": "stub/whisper",
		"audio": map[string]any{"mediaType": "audio/wav", "data": base64.StdEncoding.EncodeToString(wav(1200))}})
	add("transcriptionSchema", "json-upload", transcription.Body)
	leak("transcriptionSchema", "provider-leak", transcription.Body, "servingProvider", "stub")
	refusal := h.post("/v1/audio/speech", brokeKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
	add("publicErrorSchema", "insufficient-balance", refusal.Body)
	leak("publicErrorSchema", "provider-error-leak", refusal.Body, "providerError", map[string]any{"provider": "stub"})
	add("publicErrorSchema", "invalid-speed", h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "speed": 9}).Body)

	// The catalogue and receipts.
	add("modelListSchema", "public", h.get("/v1/models", validKey).Body)
	add("modelSchema", "one", h.get("/v1/models/openai/gpt-realtime-2.1", validKey).Body)
	add("powerLevelListSchema", "all", h.get("/v1/power-levels", validKey).Body)
	add("generationRecordSchema", "relayed", h.get("/v1/generations/gen_1", validKey).Body)

	// A realtime session with a tool call, every event it sent.
	h.upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Event{"type": "response.function_call_arguments.done", "response_id": "resp_1", "item_id": "item_call", "output_index": 0, "call_id": "call_1", "name": "lookup", "arguments": `{}`})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "output": []any{fake.Event{"type": "function_call"}}, "usage": fake.Usage(10, 0, 0, 0, 12, 0)}})
		conn.WaitForClose()
	}
	ws := h.dialRealtime(validKey, conversationOpen(nil))
	events := readUntil(t, ws, "response.done")
	sendCommand(t, ws, map[string]any{"schemaVersion": 1, "requestId": events[0].head.RequestID, "commandId": "cmd_close", "type": "session.close"})
	events = append(events, readUntil(t, ws, "session.closed")...)
	for index, read := range events {
		add("realtimePublicServerEventSchema", fmt.Sprintf("%02d-%s", index, read.head.Type), read.raw)
	}
	refused := readEvent(t, h.dialRealtime(revokedKey, conversationOpen(nil))).raw
	add("realtimePublicServerEventSchema", "refused-open", refused)
	// The realtime events tolerate an unknown top-level field (they are not
	// strict), so the control leaks inside the strict public error instead.
	var event map[string]any
	_ = json.Unmarshal(refused, &event)
	event["error"].(map[string]any)["providerError"] = map[string]any{"provider": "openai-realtime"}
	leaked, _ := json.Marshal(event)
	invalid = append(invalid, fixture{Schema: "realtimePublicServerEventSchema", Case: "error-with-provider", Value: leaked})

	if len(valid) < 30 || len(invalid) != 4 {
		t.Fatalf("captured %d shapes and %d controls; the gate needs both", len(valid), len(invalid))
	}
	root := os.Getenv("KAANA_CONTRACT_FIXTURE_DIR")
	if root == "" {
		return
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("KAANA_CONTRACT_FIXTURE_DIR must be absolute")
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("creating fresh fixture root: %v", err)
	}
	for kind, fixtures := range map[string][]fixture{"valid": valid, "invalid": invalid} {
		dir := filepath.Join(root, kind)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for index, item := range fixtures {
			encoded, _ := json.MarshalIndent(item, "", "  ")
			name := fmt.Sprintf("%02d-%s-%s.json", index, item.Schema, item.Case)
			if err := os.WriteFile(filepath.Join(dir, name), append(encoded, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
