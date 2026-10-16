package publicapi_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/publicapi"
)

// The whole path, once: the credential goes to Oxy verbatim with content-free
// facts, the grant binds the exact body, the provider is called with the
// grant's routes, the caller gets the audio and what served it, and the
// settlement reaches Oxy once, signed by Kaana, naming the step that served.
func TestSpeechIsGrantedExecutedAndSettledExactlyOnce(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := []byte(`{"model":"stub/voice","input":"Hola, mundo secreto","voice":"rachel"}`)
	response := h.post("/v1/audio/speech", validKey, body, "Idempotency-Key", "idem-speech-1", "X-Oxy-User-Id", "user_7")
	audio := readAll(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, audio)
	}
	if string(audio) != "ID3-chunk-0ID3-chunk-1ID3-chunk-2" {
		t.Errorf("audio = %q", audio)
	}
	if got := response.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := response.Header.Get(publicapi.HeaderModel); got != string(speechModel) {
		t.Errorf("Kaana-Model = %q, want the revision that served", got)
	}
	if got := response.Header.Get(publicapi.HeaderUsage); got != "characters=19" {
		t.Errorf("Kaana-Usage = %q", got)
	}
	if response.Header.Get(publicapi.HeaderContractVersion) != contract.ContractVersion || response.Header.Get(publicapi.HeaderRequestID) == "" {
		t.Error("the public headers are missing")
	}

	authorizations := h.oxy.authorizationBodies()
	if len(authorizations) != 1 {
		t.Fatalf("%d authorizations, want 1", len(authorizations))
	}
	var asked contract.InferenceAuthorizationRequest
	if err := json.Unmarshal([]byte(authorizations[0]), &asked); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(authorizations[0], "secreto") || strings.Contains(authorizations[0], "rachel") {
		t.Errorf("the authorization carried content: %s", authorizations[0])
	}
	if asked.Credential != validKey || asked.Operation != contract.GrantAudioSpeech || asked.BodySHA256 != sha256Hex(body) ||
		asked.Facts.InputCharacters == nil || *asked.Facts.InputCharacters != 19 || asked.Target == nil || asked.Target.Reference() != "stub/voice" ||
		asked.IdempotencyKey == nil || *asked.IdempotencyKey != "idem-speech-1" || asked.OnBehalfOfUserID == nil || *asked.OnBehalfOfUserID != "user_7" {
		t.Errorf("authorization = %+v", asked)
	}
	if string(asked.RequestID) != response.Header.Get(publicapi.HeaderRequestID) {
		t.Error("Oxy was asked under another request id than the caller was answered with")
	}
	call := h.adapter.lastCall()
	if call == nil || call.Attribution.Principal.Billing.AccountID != "acct_1" || call.Attribution.UserID == nil || *call.Attribution.UserID != "user_7" ||
		call.Speech == nil || call.Speech.Voice != "rachel" || call.IdempotencyKey == nil {
		t.Fatalf("the provider was called with %+v", call)
	}

	h.settle()
	settled := h.oxy.settled()
	if len(settled) != 1 {
		t.Fatalf("%d settlements, want 1", len(settled))
	}
	settlement := settled["grt_1"]
	if settlement.ServedStep == nil || *settlement.ServedStep != 0 || settlement.ReservationID == nil || *settlement.ReservationID != "rsv_1" ||
		settlement.Report.Outcome != contract.OutcomeCompleted || settlement.Report.RequestID != asked.RequestID ||
		settlement.Report.DeploymentID != "dep_speech" || len(settlement.Report.Units) != 1 {
		t.Errorf("settlement = %+v", settlement)
	}
	rows := h.outbox.snapshot()
	if len(rows) != 1 || rows[0].delivered != "settled" {
		t.Fatalf("outbox = %+v", rows)
	}
	// Nothing is due any more: a second pass delivers nothing.
	h.settle()
	if h.oxy.deliveries != 1 {
		t.Errorf("%d deliveries, want 1", h.oxy.deliveries)
	}
}

// A lost acknowledgement is retried until Oxy answers, and Oxy's idempotency
// on the grant makes the retry a duplicate, never a second charge.
func TestASettlementWhoseAcknowledgementIsLostIsRetriedAsADuplicate(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.oxy.loseAcknowledgements = 1
	first := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"})
	readAll(t, first)
	h.settle()
	if rows := h.outbox.snapshot(); rows[0].delivered != "" {
		t.Fatalf("a delivery whose acknowledgement was lost was marked delivered: %+v", rows[0])
	}
	h.outbox.makeDue()
	h.settler.DeliverDue(t.Context())
	rows := h.outbox.snapshot()
	if rows[0].delivered != "duplicate" || rows[0].attempts != 2 {
		t.Fatalf("after the retry: %+v", rows[0])
	}
	if len(h.oxy.settled()) != 1 || h.oxy.deliveries != 2 {
		t.Errorf("Oxy settled %d grants from %d deliveries", len(h.oxy.settled()), h.oxy.deliveries)
	}
}

// Oxy being down settles nothing and reaches no provider: there is no
// degraded mode without a grant. A refusal reaches the caller as the public
// code, with Oxy's own wording (which can name the account) replaced.
func TestNoGrantNoProvider(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	request := map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"}

	revoked := h.post("/v1/audio/speech", revokedKey, request)
	failure := publicError(t, revoked, http.StatusUnauthorized)
	if failure.Code != "authentication_failed" || !strings.Contains(failure.Message, "https://console.oxy.so") || failure.Retryable {
		t.Errorf("revoked credential = %+v", failure)
	}
	broke := h.post("/v1/audio/speech", brokeKey, request)
	failure = publicError(t, broke, http.StatusPaymentRequired)
	if failure.Code != "insufficient_balance" || strings.Contains(failure.Message, "acct_secret_detail") {
		t.Errorf("insufficient balance = %+v", failure)
	}
	failure = publicError(t, h.post("/v1/audio/speech", "", request), http.StatusUnauthorized)
	if failure.Code != "authentication_failed" {
		t.Errorf("no credential = %+v", failure)
	}
	h.oxy.mu.Lock()
	h.oxy.down = true
	h.oxy.mu.Unlock()
	response := h.post("/v1/audio/speech", validKey, request)
	failure = publicError(t, response, http.StatusServiceUnavailable)
	if failure.Code != "service_unavailable" || !failure.Retryable || response.Header.Get("Retry-After") == "" {
		t.Errorf("Oxy down = %+v", failure)
	}
	h.settle()
	if h.adapter.called() != 0 || len(h.outbox.snapshot()) != 0 {
		t.Errorf("without a grant: %d provider calls, %d settlements", h.adapter.called(), len(h.outbox.snapshot()))
	}
}

// A grant that does not verify, or is for other bytes, is not executed.
func TestAGrantThatDoesNotBindThisRequestIsRefused(t *testing.T) {
	for name, mutate := range map[string]func(*contract.Grant){
		"other body":      func(g *contract.Grant) { g.BodySHA256 = strings.Repeat("0", 64) },
		"other request":   func(g *contract.Grant) { g.RequestID = "req_someone_else" },
		"other operation": func(g *contract.Grant) { g.Operation = contract.GrantResponses },
		"unknown field":   nil,
		"outside window": func(g *contract.Grant) {
			g.ExecuteBy = g.IssuedAt
			g.IssuedAt = "2026-01-01T00:00:00Z"
			g.ExecuteBy = "2026-01-01T00:01:00Z"
		},
		"charged, no hold": func(g *contract.Grant) { g.Reservation = nil },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{})
			if mutate == nil {
				h.oxy.mutate = func(g *contract.Grant) { g.Client.Labels = contract.RequestLabels{"x": "y"} }
				// A signature by a key Kaana does not trust stands in for a
				// forged grant.
				h.oxy.key = append([]byte(nil), h.oxy.key...)
				for index := range h.oxy.key[:32] {
					h.oxy.key[index] ^= 0xff
				}
			} else {
				h.oxy.mutate = mutate
			}
			failure := publicError(t, h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"}), http.StatusServiceUnavailable)
			if failure.Code != "service_unavailable" {
				t.Errorf("failure = %+v", failure)
			}
			if h.adapter.called() != 0 {
				t.Error("a provider was called under a grant that does not bind the request")
			}
		})
	}
}

type publishedAudio struct{}

func (publishedAudio) Audio(model contract.ModelReference) (*publicapi.AudioCapabilities, bool) {
	if model.ModelID() != speechModel.ModelID() {
		return nil, false
	}
	limit := 50
	return &publicapi.AudioCapabilities{Voices: []string{"rachel"}, OutputMediaTypes: []contract.AudioMediaType{"audio/mpeg", "audio/pcm"},
		Speed: &publicapi.SpeedRange{Min: 0.7, Max: 1.2}, MaxInputCharacters: &limit}, true
}

// A speech request a model refuses is refused before anything is held: no
// authorization is asked for (ADR 0002, decision 9).
func TestSpeechIsValidatedAgainstTheModelBeforeAnyGrant(t *testing.T) {
	h := newHarness(t, harnessOptions{audio: publishedAudio{}})
	for _, test := range []struct {
		body  map[string]any
		param string
	}{
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "speed": 1.5}, "speed"},
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "nobody"}, "voice"},
		{map[string]any{"model": "stub/voice", "input": strings.Repeat("a", 51), "voice": "rachel"}, "input"},
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "response_format": "wav"}, "response_format"},
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "speed": 9}, "speed"},
		{map[string]any{"model": "high", "input": "Hola", "voice": "rachel"}, "model"},
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "stream": true, "response_format": "flac"}, "response_format"},
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "stream_format": "sse"}, "stream_format"},
		{map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "instructions": "whisper"}, "instructions"},
	} {
		failure := publicError(t, h.post("/v1/audio/speech", validKey, test.body), http.StatusBadRequest)
		if failure.Code != "invalid_request" || failure.Param == nil || *failure.Param != test.param {
			t.Errorf("%v: %+v", test.body, failure)
		}
	}
	if len(h.oxy.authorizationBodies()) != 0 {
		t.Errorf("%d authorizations were asked for invalid requests", len(h.oxy.authorizationBodies()))
	}
	// Positive control: the same model accepts what it publishes.
	response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "speed": 1.1})
	if readAll(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("a request inside the published limits got %d", response.StatusCode)
	}
}

// Streamed speech as typed SSE: deltas as they arrive, then done with what
// served and what it consumed. A failure after the first byte arrives typed,
// with no provider in it, and the settlement keeps what was consumed.
func TestStreamedSpeechOverSSEAndAFailureMidStream(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	request := map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "stream": true, "stream_format": "sse"}
	response := h.post("/v1/audio/speech", validKey, request)
	frames := readSSE(t, response)
	if len(frames) != 4 || frames[0].event != "speech.audio.delta" || frames[3].event != "speech.audio.done" {
		t.Fatalf("frames = %+v", frames)
	}
	var done contract.SpeechAudioDoneEvent
	_ = json.Unmarshal(frames[3].data, &done)
	if done.Model != speechModel || len(done.Usage) != 1 || string(done.RequestID) != response.Header.Get(publicapi.HeaderRequestID) {
		t.Errorf("done = %+v", done)
	}

	h.adapter.failAfter = 2
	frames = readSSE(t, h.post("/v1/audio/speech", validKey, request))
	if len(frames) != 3 || frames[2].event != "error" {
		t.Fatalf("frames after a mid-stream failure = %+v", frames)
	}
	assertNoProviderLeak(t, "the error frame", frames[2].data)
	var failure contract.SpeechStreamErrorEvent
	_ = json.Unmarshal(frames[2].data, &failure)
	if failure.Error.Code != "provider_error" || failure.Error.Message != contract.PublicMessage("provider_error") {
		t.Errorf("error frame = %+v", failure)
	}
	h.settle()
	settlement := h.oxy.settled()["grt_2"]
	if settlement.Report.Outcome != contract.OutcomePartial || len(settlement.Report.Units) == 0 || settlement.ServedStep == nil {
		t.Errorf("a stream that failed after output settles what it consumed: %+v", settlement)
	}
}

// Raw streamed audio (the OpenAI-compatible form): bytes as they arrive; a
// failure after the first byte cuts the connection.
func TestStreamedSpeechAsRawAudio(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	request := map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel", "stream": true, "response_format": "pcm"}
	response := h.post("/v1/audio/speech", validKey, request)
	audio := readAll(t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "audio/pcm;rate=24000" || string(audio) != "ID3-chunk-0ID3-chunk-1ID3-chunk-2" {
		t.Fatalf("%d %q %q", response.StatusCode, response.Header.Get("Content-Type"), audio)
	}
	h.adapter.failAfter = 1
	response = h.post("/v1/audio/speech", validKey, request)
	if response.ReadErr == nil {
		t.Errorf("a stream that failed after its first byte ended cleanly with %q", response.Body)
	}
}

type sseFrame struct {
	event string
	data  []byte
}

func readSSE(t *testing.T, response *reply) []sseFrame {
	t.Helper()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("not an event stream: %d %s", response.StatusCode, response.Body)
	}
	var frames []sseFrame
	scanner := bufio.NewScanner(bytes.NewReader(response.Body))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var current sseFrame
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			current.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.data = []byte(strings.TrimPrefix(line, "data: "))
		case line == "" && current.data != nil:
			frames = append(frames, current)
			current = sseFrame{}
		}
	}
	return frames
}

// wav is a minimal 16 kHz mono PCM16 WAV of the given duration.
func wav(milliseconds int) []byte {
	samples := 16 * milliseconds
	var buffer bytes.Buffer
	buffer.WriteString("RIFF")
	_ = binary.Write(&buffer, binary.LittleEndian, uint32(36+samples*2))
	buffer.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&buffer, binary.LittleEndian, field)
	}
	buffer.WriteString("data")
	_ = binary.Write(&buffer, binary.LittleEndian, uint32(samples*2))
	buffer.Write(make([]byte, samples*2))
	return buffer.Bytes()
}

// Transcription in both forms: the measured duration reaches Oxy as a fact,
// the audio reaches the provider inline, and the response is the text.
func TestTranscriptionAsMultipartAndAsJSON(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	audio := wav(1500)

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("model", "stub/whisper")
	part, _ := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="audio.wav"`}, "Content-Type": {"audio/wav"},
	})
	_, _ = part.Write(audio)
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/audio/transcriptions", &form)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+validKey)
	response := h.do(request)
	body := readAll(t, response)
	var transcription contract.Transcription
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &transcription) != nil || transcription.Text != "hola mundo" ||
		transcription.Model != whisperModel || transcription.DurationMs == nil || *transcription.DurationMs != 1500 {
		t.Fatalf("multipart: %d %s", response.StatusCode, body)
	}
	assertNoProviderLeak(t, "a transcription", body)
	var asked contract.InferenceAuthorizationRequest
	_ = json.Unmarshal([]byte(h.oxy.authorizationBodies()[0]), &asked)
	if asked.Facts.AudioInputMilliseconds == nil || *asked.Facts.AudioInputMilliseconds != 1500 || asked.Operation != contract.GrantAudioTranscriptions {
		t.Errorf("authorization facts = %+v", asked.Facts)
	}
	call := h.adapter.lastCall()
	if call.Client.APIFormat != contract.APIFormatAudioTranscriptions || *call.Input.Messages[0].Content[0].Source.Data != base64.StdEncoding.EncodeToString(audio) {
		t.Error("the provider did not receive the uploaded audio inline")
	}

	jsonBody := map[string]any{"model": "stub/whisper", "audio": map[string]any{"mediaType": "audio/wav", "data": base64.StdEncoding.EncodeToString(audio)}}
	response = h.post("/v1/audio/transcriptions", validKey, jsonBody)
	body = readAll(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"text":"hola mundo"`) {
		t.Fatalf("json: %d %s", response.StatusCode, body)
	}

	// The language travels in the envelope (contract set 4.5.0), never
	// dropped on the way to the adapter.
	jsonBody["language"] = "es-ES"
	response = h.post("/v1/audio/transcriptions", validKey, jsonBody)
	body = readAll(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("with a language: %d %s", response.StatusCode, body)
	}
	if call := h.adapter.lastCall(); call.Transcription == nil || call.Transcription.Language == nil || *call.Transcription.Language != "es-ES" {
		t.Errorf("the language did not reach the adapter: %+v", call.Transcription)
	}

	// Any language and a prompt are the adapter's to translate (Kaana ADR
	// 0003): a language the model does not list and a prompt reach it as
	// they were sent.
	jsonBody["language"], jsonBody["prompt"] = "gl", "names: Kaana"
	if response := h.post("/v1/audio/transcriptions", validKey, jsonBody); response.StatusCode != http.StatusOK {
		t.Fatalf("with an unlisted language and a prompt: %d %s", response.StatusCode, readAll(t, response))
	}
	if call := h.adapter.lastCall(); call.Transcription == nil || *call.Transcription.Language != "gl" || *call.Transcription.Prompt != "names: Kaana" {
		t.Errorf("the adapter received %+v", call.Transcription)
	}
	delete(jsonBody, "prompt")

	// Refused before a grant: a tag that is not one, an upload the model
	// does not read, and audio longer than the ceiling.
	before := len(h.oxy.authorizationBodies())
	jsonBody["language"] = "español"
	if failure := publicError(t, h.post("/v1/audio/transcriptions", validKey, jsonBody), http.StatusBadRequest); *failure.Param != "language" {
		t.Errorf("language español = %+v", failure)
	}
	delete(jsonBody, "language")
	upload := jsonBody["audio"]
	jsonBody["audio"] = map[string]any{"mediaType": "audio/flac", "data": base64.StdEncoding.EncodeToString(wav(1000))}
	if failure := publicError(t, h.post("/v1/audio/transcriptions", validKey, jsonBody), http.StatusBadRequest); *failure.Param != "file" {
		t.Errorf("an upload the model does not read = %+v", failure)
	}
	jsonBody["audio"] = upload
	jsonBody["audio"] = map[string]any{"mediaType": "audio/wav", "data": base64.StdEncoding.EncodeToString(wav(31 * 60 * 1000))}
	if failure := publicError(t, h.post("/v1/audio/transcriptions", validKey, jsonBody), http.StatusRequestEntityTooLarge); failure.Code != "request_too_large" {
		t.Errorf("31 minutes = %+v", failure)
	}
	// Raw PCM states no rate: its duration cannot be measured, so nothing is
	// held for it and it is refused rather than guessed.
	jsonBody["audio"] = map[string]any{"mediaType": "audio/pcm", "data": base64.StdEncoding.EncodeToString(make([]byte, 3200))}
	if failure := publicError(t, h.post("/v1/audio/transcriptions", validKey, jsonBody), http.StatusBadRequest); failure.Param == nil || *failure.Param != "file" {
		t.Errorf("unmeasurable audio = %+v", failure)
	}
	jsonBody["audio"] = map[string]any{"mediaType": "video/mp4", "data": "AAAA"}
	publicError(t, h.post("/v1/audio/transcriptions", validKey, jsonBody), http.StatusBadRequest)
	if len(h.oxy.authorizationBodies()) != before {
		t.Error("an invalid transcription asked Oxy for a grant")
	}
	h.settle()
	if len(h.oxy.settled()) != 4 {
		t.Errorf("%d settlements for 4 transcriptions", len(h.oxy.settled()))
	}
}

// One credential flooding the API is refused locally, before Oxy is asked.
func TestTheLocalRateLimitRefusesBeforeOxyIsAsked(t *testing.T) {
	h := newHarness(t, harnessOptions{rate: publicapi.RateLimit{PerSecond: 0.001, Burst: 1}})
	request := map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"}
	readAll(t, h.post("/v1/audio/speech", validKey, request))
	response := h.post("/v1/audio/speech", validKey, request)
	failure := publicError(t, response, http.StatusTooManyRequests)
	if failure.Code != "rate_limited" || !failure.Retryable || failure.RetryAfterMs == nil || response.Header.Get("Retry-After") == "" {
		t.Errorf("rate limited = %+v", failure)
	}
	// Another credential is its own bucket.
	if response := h.post("/v1/audio/speech", otherKey, request); response.StatusCode != http.StatusOK {
		t.Errorf("another credential was limited: %d", response.StatusCode)
	}
	if len(h.oxy.authorizationBodies()) != 2 {
		t.Errorf("%d authorizations; the limited request must not reach Oxy", len(h.oxy.authorizationBodies()))
	}
}

// A browser origin Console's playground uses gets CORS; any other none.
func TestCORSAdmitsOnlyTheConfiguredOrigins(t *testing.T) {
	h := newHarness(t, harnessOptions{origins: []string{"https://console.oxy.so"}})
	preflight := func(origin string) *reply {
		request, _ := http.NewRequest(http.MethodOptions, h.server.URL+"/v1/audio/speech", nil)
		request.Header.Set("Origin", origin)
		request.Header.Set("Access-Control-Request-Method", "POST")
		request.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
		return h.do(request)
	}
	allowed := preflight("https://console.oxy.so")
	if allowed.StatusCode != http.StatusNoContent || allowed.Header.Get("Access-Control-Allow-Origin") != "https://console.oxy.so" ||
		!strings.Contains(allowed.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("console preflight = %d %v", allowed.StatusCode, allowed.Header)
	}
	if denied := preflight("https://evil.example"); denied.StatusCode != http.StatusForbidden || denied.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("another origin = %d %v", denied.StatusCode, denied.Header)
	}
	response := h.post("/v1/audio/speech", validKey, map[string]any{"model": "stub/voice", "input": "Hola", "voice": "rachel"}, "Origin", "https://console.oxy.so")
	readAll(t, response)
	if !strings.Contains(response.Header.Get("Access-Control-Expose-Headers"), publicapi.HeaderUsage) {
		t.Errorf("the playground cannot read Kaana-Usage: %v", response.Header)
	}
}
