package realtime_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	fake "github.com/OxyHQ/Kaana/internal/provider/openairealtime/openairealtimetest"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// spokenTurn is the fake's answer to one appended frame: a response with
// audio, and its usage.
func spokenTurn(conn *fake.Conn, responseID string, usage fake.Event) {
	conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": responseID, "status": "in_progress"}})
	conn.Send(fake.Event{"type": "response.output_audio_transcript.delta", "response_id": responseID, "item_id": "item_" + responseID, "content_index": 0, "delta": "Hi"})
	conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": responseID, "item_id": "item_" + responseID, "content_index": 0, "delta": "AAAAAAAA"})
	conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": responseID, "status": "completed", "output": []any{}, "usage": usage}})
}

// TestASessionOpensStreamsAndSettlesExactlyOnce is the whole lifecycle on the
// real wire at both ends: signed first frame, session.created, a command
// acknowledged before it is applied, provider events renumbered into one
// sequence, session.closed with the totals, exactly one usage report, close
// 1000, and exactly one operator cost record for the session.
func TestASessionOpensStreamsAndSettlesExactlyOnce(t *testing.T) {
	h := newHarness(t, options{})
	h.upstream.Script = func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		spokenTurn(conn, "resp_1", fake.Usage(119, 13, 64, 0, 30, 91))
		conn.Expect("input_audio_buffer.append")
		spokenTurn(conn, "resp_2", fake.Usage(10, 20, 0, 5, 1, 2))
	}
	const requestID = contract.RequestID("req_lifecycle")
	ws := h.open(sessionRequest(requestID, primary))

	created := read(t, ws)
	if created.kind() != "session.created" || created.sequence() != 0 || created["deploymentId"] != string(primary) ||
		created["servingProvider"] != "openai-realtime" || created["resumeWindowMs"] != float64(30000) || created["resolvedModelReference"] != string(modelReference) {
		t.Fatalf("session.created = %v", created)
	}
	frames := []frame{created}
	command(t, ws, appendAudio(requestID, "cmd_1", "AAAA"))
	turn := readUntil(t, ws, "response.done")
	if turn[0].kind() != "command.accepted" || turn[0]["commandId"] != "cmd_1" || turn[0]["duplicate"] != false {
		t.Fatalf("the command was not acknowledged first: %s", describe(turn))
	}
	frames = append(frames, turn...)
	command(t, ws, appendAudio(requestID, "cmd_2", "AAAA"))
	frames = append(frames, readUntil(t, ws, "response.done")...)
	command(t, ws, closeCommand(requestID, "cmd_close"))
	frames = append(frames, readUntil(t, ws, "session.closed")...)
	requireContiguous(t, frames, 0)

	closed := frames[len(frames)-1]
	wantUnits := map[string]int{"input_tokens": 55 + 10, "cached_input_tokens": 64, "audio_input_tokens": 13 + 15, "cached_audio_input_tokens": 5, "output_tokens": 31, "audio_output_tokens": 93}
	if closed["reason"] != "client_closed" || closed["deploymentId"] != string(primary) || !reflect.DeepEqual(unitsOf(closed["units"]), wantUnits) {
		t.Fatalf("session.closed = %v", closed)
	}

	report := read(t, ws)
	if report["requestId"] != string(requestID) || report["outcome"] != "completed" || report["deploymentId"] != string(primary) ||
		report["servingProvider"] != "openai-realtime" || report["generationId"] == nil || report["routeSwitches"] != float64(0) ||
		!reflect.DeepEqual(unitsOf(report["units"]), wantUnits) || report["usageSource"] != "provider_reported" {
		t.Fatalf("usage report = %v", report)
	}
	expectClose(t, ws, websocket.StatusNormalClosure)

	waitFor(t, "the cost record", func() bool { return len(h.costs.recorded()) == 1 })
	event := h.costs.recorded()[0]
	if event.RequestID != requestID || event.KeyID != "key_a" || event.DeploymentID != primary || !event.Served ||
		event.Telemetry.Outcome != providercost.AttemptSucceeded || event.Telemetry.Latency <= 0 ||
		event.Telemetry.TimeToFirstOutput <= 0 || event.Source != providercost.SourceRateCard || !event.Complete {
		t.Errorf("cost event = %+v", event)
	}
	h.upstream.WaitClosed(t)
	if got := h.upstream.ReceivedTypes(); !reflect.DeepEqual(got, []string{"session.update", "input_audio_buffer.append", "input_audio_buffer.append"}) {
		t.Errorf("the provider received %v", got)
	}
	if strings.Contains(h.logs.String(), "AAAA") || strings.Contains(h.logs.String(), "be brief") {
		t.Error("session content reached a log line")
	}
	waitFor(t, "the settled session to be released", func() bool { return h.manager.Open() == 0 })
}

// TestSignatureFailuresCloseWithPolicyViolationAndNothingElse, with a
// correctly signed control on the same harness.
func TestSignatureFailuresCloseWithPolicyViolationAndNothingElse(t *testing.T) {
	h := newHarness(t, options{})
	request := encode(t, sessionRequest("req_signed", primary))
	_, otherKey, _ := ed25519.GenerateKey(nil)
	cases := map[string]func() *websocket.Conn{
		"another signing purpose": func() *websocket.Conn {
			return h.connect(request, edgeauth.CredentialControlSigningInput, nil)
		},
		"a tampered first frame": func() *websocket.Conn {
			signed := request
			ws := h.connectSignedOver(signed, bytes.Replace(signed, []byte("req_signed"), []byte("req_forged"), 1))
			return ws
		},
		"an unknown key": func() *websocket.Conn { return h.connect(request, nil, otherKey) },
		"not a session request": func() *websocket.Conn {
			return h.connect([]byte(`{"type":"input_audio.append"}`), nil, nil)
		},
		"an invalid session request": func() *websocket.Conn {
			invalid := sessionRequest("req_invalid", primary)
			invalid.Limits.MaxResponses = 0
			return h.connect(encode(t, invalid), nil, nil)
		},
	}
	for name, dial := range cases {
		t.Run(name, func(t *testing.T) {
			expectClose(t, dial(), websocket.StatusPolicyViolation)
		})
	}
	if h.upstream.Dials() != 0 {
		t.Fatalf("a refused connection reached the provider %d times", h.upstream.Dials())
	}
	// Positive control: the same request, correctly signed, opens.
	ws := h.connect(request, nil, nil)
	if created := read(t, ws); created.kind() != "session.created" {
		t.Fatalf("the signed control did not open: %v", created)
	}
}

// connectSignedOver signs one frame and sends another.
func (h *harness) connectSignedOver(signed, sent []byte) *websocket.Conn {
	h.t.Helper()
	milliseconds := time.Now().UnixMilli()
	header := http.Header{}
	header.Set(edgeauth.HeaderKeyID, h.keyID)
	header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(h.private, edgeauth.SigningInput(h.keyID, milliseconds, signed))))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/internal/v1/realtime", &websocket.DialOptions{HTTPHeader: header})
	closeBody(response)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = ws.CloseNow() })
	if err := ws.Write(ctx, websocket.MessageText, sent); err != nil {
		h.t.Fatal(err)
	}
	return ws
}

func TestABinaryFirstFrameIsRefused(t *testing.T) {
	h := newHarness(t, options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/internal/v1/realtime", nil)
	closeBody(response)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	if err := ws.Write(ctx, websocket.MessageBinary, encode(t, sessionRequest("req_binary", primary))); err != nil {
		t.Fatal(err)
	}
	expectClose(t, ws, websocket.StatusPolicyViolation)
}

// TestADuplicateCommandIsAcknowledgedAndNeverAppliedTwice: the same commandId
// again is answered duplicate:true, and the provider reads it once.
func TestADuplicateCommandIsAcknowledgedAndNeverAppliedTwice(t *testing.T) {
	h := newHarness(t, options{})
	const requestID = contract.RequestID("req_duplicate")
	ws := h.open(sessionRequest(requestID, primary))
	read(t, ws)
	command(t, ws, appendAudio(requestID, "cmd_once", "AAAA"))
	first := read(t, ws)
	command(t, ws, appendAudio(requestID, "cmd_once", "AAAA"))
	second := read(t, ws)
	if first["duplicate"] != false || second["duplicate"] != true || second["commandId"] != "cmd_once" {
		t.Fatalf("acknowledgements = %v then %v", first, second)
	}
	command(t, ws, closeCommand(requestID, "cmd_close"))
	readUntil(t, ws, "session.closed")
	h.upstream.WaitClosed(t)
	if got := h.upstream.ReceivedTypes(); !reflect.DeepEqual(got, []string{"session.update", "input_audio_buffer.append"}) {
		t.Errorf("the provider received %v; a duplicate must not be applied again", got)
	}
}

// TestResumeReplaysAfterTheNamedSequenceAndNeverDoubleMeters drops the
// connection mid-response, lets the provider keep talking, and resumes.
func TestResumeReplaysAfterTheNamedSequenceAndNeverDoubleMeters(t *testing.T) {
	h := newHarness(t, options{})
	release := make(chan struct{})
	h.upstream.Script = func(conn *fake.Conn) {
		conn.Expect("input_audio_buffer.append")
		conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress"}})
		<-release
		conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_1", "content_index": 0, "delta": "AAAAAAAA"})
		conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "usage": fake.Usage(5, 5, 0, 0, 5, 5)}})
	}
	const requestID = contract.RequestID("req_resume")
	request := sessionRequest(requestID, primary)
	first := h.open(request)
	read(t, first) // 0 session.created
	command(t, first, appendAudio(requestID, "cmd_1", "AAAA"))
	accepted := read(t, first) // 1 command.accepted
	created := read(t, first)  // 2 response.created
	if accepted.sequence() != 1 || created.kind() != "response.created" {
		t.Fatalf("before the drop: %v, %v", accepted, created)
	}
	_ = first.CloseNow()
	// The provider keeps talking while nobody is attached; its events are
	// buffered for the resume.
	close(release)
	time.Sleep(100 * time.Millisecond)

	// The client processed sequence 1 and missed everything after it.
	second := h.connect(encodeCommand(t, resumeCommand(requestID, "cmd_resume", 1)), nil, nil)
	replayed := readUntil(t, second, "session.resumed")
	if describe(replayed) != "2:response.created 3:output_audio.delta 4:response.done 5:session.resumed" {
		t.Fatalf("replay = %s", describe(replayed))
	}
	if resumed := replayed[len(replayed)-1]; resumed["afterSequence"] != float64(1) || resumed["commandId"] != "cmd_resume" {
		t.Errorf("session.resumed = %v", resumed)
	}
	command(t, second, closeCommand(requestID, "cmd_close"))
	closing := readUntil(t, second, "session.closed")
	requireContiguous(t, closing, 6)
	report := read(t, second)
	if report["outcome"] != "completed" || unitsOf(report["units"])["audio_output_tokens"] != 5 {
		t.Fatalf("report = %v", report)
	}
	expectClose(t, second, websocket.StatusNormalClosure)
	h.upstream.WaitClosed(t)
	if got := h.upstream.ReceivedTypes(); !reflect.DeepEqual(got, []string{"session.update", "input_audio_buffer.append"}) {
		t.Errorf("the provider received %v; a reconnect must never replay a command", got)
	}
	waitFor(t, "the cost record", func() bool { return len(h.costs.recorded()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if records := h.costs.recorded(); len(records) != 1 {
		t.Fatalf("%d cost records for one session", len(records))
	}
}

func TestResumeRefusals(t *testing.T) {
	t.Run("an unknown session", func(t *testing.T) {
		h := newHarness(t, options{})
		ws := h.connect(encodeCommand(t, resumeCommand("req_nobody", "cmd_resume", -1)), nil, nil)
		refusal := read(t, ws)
		failure, _ := refusal["error"].(map[string]any)
		if refusal.kind() != "error" || refusal["fatal"] != true || failure["code"] != "invalid_request" || refusal["commandId"] != "cmd_resume" {
			t.Fatalf("refusal = %v", refusal)
		}
		expectClose(t, ws, websocket.StatusPolicyViolation)
	})

	t.Run("a sequence no longer buffered", func(t *testing.T) {
		h := newHarness(t, options{replayEvents: 2})
		const requestID = contract.RequestID("req_forgotten")
		ws := h.open(sessionRequest(requestID, primary))
		read(t, ws)
		for index := range 4 {
			command(t, ws, &contract.RealtimeInputAudioClearCommand{SchemaVersion: 1, RequestID: requestID, CommandID: contract.RealtimeCommandID("cmd_" + strconv.Itoa(index))})
			read(t, ws)
		}
		refused := h.connect(encodeCommand(t, resumeCommand(requestID, "cmd_resume", 0)), nil, nil)
		if refusal := read(t, refused); refusal.kind() != "error" || refusal["fatal"] != true {
			t.Fatalf("refusal = %v", refusal)
		}
		expectClose(t, refused, websocket.StatusPolicyViolation)
		// Control: the latest sequence is still buffered and resumes.
		resumed := h.connect(encodeCommand(t, resumeCommand(requestID, "cmd_resume_2", 4)), nil, nil)
		if frame := read(t, resumed); frame.kind() != "session.resumed" || frame.sequence() != 5 {
			t.Fatalf("control resume = %v", frame)
		}
	})

	t.Run("an expired window", func(t *testing.T) {
		h := newHarness(t, options{resumeWindow: 200 * time.Millisecond})
		const requestID = contract.RequestID("req_expired")
		ws := h.open(sessionRequest(requestID, primary))
		read(t, ws)
		_ = ws.CloseNow()
		waitFor(t, "the window to expire", func() bool { return h.manager.Open() == 0 })
		late := h.connect(encodeCommand(t, resumeCommand(requestID, "cmd_resume", 0)), nil, nil)
		if refusal := read(t, late); refusal.kind() != "error" || refusal["fatal"] != true {
			t.Fatalf("refusal = %v", refusal)
		}
		expectClose(t, late, websocket.StatusPolicyViolation)
		if !strings.Contains(h.logs.String(), `"reason":"resume_expired"`) {
			t.Error("the abandoned session did not close with resume_expired")
		}
	})
}

// TestLimitsAreEnforcedExactly covers every signed ceiling. Each closes the
// session with the reason the wire names, and the one a command would pass is
// refused before it is acknowledged or sent.
func TestLimitsAreEnforcedExactly(t *testing.T) {
	t.Run("input audio", func(t *testing.T) {
		h := newHarness(t, options{})
		const requestID = contract.RequestID("req_input_limit")
		request := sessionRequest(requestID, primary)
		request.Limits.MaxInputAudioBytes = 6 // two frames of three bytes
		ws := h.open(request)
		read(t, ws)
		command(t, ws, appendAudio(requestID, "cmd_1", "AAAA"))
		command(t, ws, appendAudio(requestID, "cmd_2", "AAAA"))
		if a, b := read(t, ws), read(t, ws); a["commandId"] != "cmd_1" || b["commandId"] != "cmd_2" {
			t.Fatalf("the frames within the ceiling were not accepted: %v %v", a, b)
		}
		command(t, ws, appendAudio(requestID, "cmd_3", "AA=="))
		frames := readUntil(t, ws, "session.closed")
		if describe(frames) != "3:error 4:session.closed" || frames[0]["commandId"] != "cmd_3" || frames[0]["fatal"] != true || frames[1]["reason"] != "limit_exceeded" {
			t.Fatalf("frames = %s %v", describe(frames), frames)
		}
		read(t, ws) // the usage report
		expectClose(t, ws, websocket.StatusNormalClosure)
		h.upstream.WaitClosed(t)
		if got := len(h.upstream.ReceivedTypes()); got != 3 {
			t.Errorf("the provider read %d events; the refused frame must never be sent", got)
		}
	})

	t.Run("output audio", func(t *testing.T) {
		h := newHarness(t, options{})
		h.upstream.Script = func(conn *fake.Conn) {
			conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress"}})
			conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_1", "content_index": 0, "delta": "AAAAAAAA"})
			conn.Send(fake.Event{"type": "response.output_audio.delta", "response_id": "resp_1", "item_id": "item_1", "content_index": 0, "delta": "AAAAAAAA"})
		}
		request := sessionRequest("req_output_limit", primary)
		request.Limits.MaxOutputAudioBytes = 8 // one six-byte frame fits, two do not
		ws := h.open(request)
		frames := readUntil(t, ws, "session.closed")
		if describe(frames) != "0:session.created 1:response.created 2:output_audio.delta 3:error 4:session.closed" || frames[4]["reason"] != "limit_exceeded" {
			t.Fatalf("frames = %s", describe(frames))
		}
	})

	t.Run("responses", func(t *testing.T) {
		h := newHarness(t, options{})
		h.upstream.Script = func(conn *fake.Conn) {
			conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress"}})
			conn.Send(fake.Event{"type": "response.done", "response": fake.Event{"id": "resp_1", "status": "completed", "usage": fake.Usage(1, 0, 0, 0, 1, 0)}})
			conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_2", "status": "in_progress"}})
		}
		request := sessionRequest("req_response_limit", primary)
		request.Limits.MaxResponses = 1
		ws := h.open(request)
		frames := readUntil(t, ws, "session.closed")
		if describe(frames) != "0:session.created 1:response.created 2:response.done 3:error 4:session.closed" || frames[4]["reason"] != "limit_exceeded" {
			t.Fatalf("frames = %s", describe(frames))
		}
		// The response.done the provider reported before the ceiling is
		// metered; the refused response never is.
		if report := read(t, ws); report["outcome"] != "completed" || unitsOf(report["units"])["output_tokens"] != 1 {
			t.Errorf("report = %v", report)
		}
	})

	t.Run("response.create at the ceiling", func(t *testing.T) {
		h := newHarness(t, options{})
		h.upstream.Script = func(conn *fake.Conn) {
			conn.Send(fake.Event{"type": "response.created", "response": fake.Event{"id": "resp_1", "status": "in_progress"}})
		}
		const requestID = contract.RequestID("req_create_limit")
		request := sessionRequest(requestID, primary)
		request.Limits.MaxResponses = 1
		ws := h.open(request)
		readUntil(t, ws, "response.created")
		command(t, ws, &contract.RealtimeResponseCreateCommand{SchemaVersion: 1, RequestID: requestID, CommandID: "cmd_more"})
		frames := readUntil(t, ws, "session.closed")
		if frames[0].kind() != "error" || frames[0]["commandId"] != "cmd_more" || frames[1]["reason"] != "limit_exceeded" {
			t.Fatalf("frames = %s", describe(frames))
		}
	})

	t.Run("idle timeout", func(t *testing.T) {
		h := newHarness(t, options{})
		request := sessionRequest("req_idle", primary)
		request.Limits.IdleTimeoutMs = 1000
		ws := h.open(request)
		started := time.Now()
		frames := readUntil(t, ws, "session.closed")
		if frames[len(frames)-1]["reason"] != "idle_timeout" || time.Since(started) < 900*time.Millisecond {
			t.Fatalf("frames = %s after %s", describe(frames), time.Since(started))
		}
	})

	t.Run("max duration", func(t *testing.T) {
		h := newHarness(t, options{})
		const requestID = contract.RequestID("req_duration")
		request := sessionRequest(requestID, primary)
		request.Limits.MaxDurationMs, request.Limits.IdleTimeoutMs = 1200, 1000
		ws := h.open(request)
		read(t, ws)
		started := time.Now()
		// Commands keep the session from going idle; only its duration can
		// end it.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for index := 0; ; index++ {
				select {
				case <-stop:
					return
				case <-time.After(200 * time.Millisecond):
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = ws.Write(ctx, websocket.MessageText, encodeCommand(t, &contract.RealtimeInputAudioClearCommand{SchemaVersion: 1, RequestID: requestID, CommandID: contract.RealtimeCommandID("cmd_" + strconv.Itoa(index))}))
				cancel()
			}
		}()
		frames := readUntil(t, ws, "session.closed")
		if frames[len(frames)-1]["reason"] != "max_duration" || time.Since(started) < time.Second {
			t.Fatalf("closed with %v after %s", frames[len(frames)-1]["reason"], time.Since(started))
		}
	})
}

// TestABinaryFrameMidSessionEndsIt: error, session.closed, the report, and
// close 1003.
func TestABinaryFrameMidSessionEndsIt(t *testing.T) {
	h := newHarness(t, options{})
	ws := h.open(sessionRequest("req_binary_mid", primary))
	read(t, ws)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	frames := readUntil(t, ws, "session.closed")
	if describe(frames) != "1:error 2:session.closed" || frames[0]["fatal"] != true {
		t.Fatalf("frames = %s", describe(frames))
	}
	if report := read(t, ws); report["requestId"] != "req_binary_mid" {
		t.Fatalf("report = %v", report)
	}
	expectClose(t, ws, websocket.StatusUnsupportedData)
}

// TestFailoverHappensOnlyBeforeTheSessionOpens: the first signed route's key
// is refused, so the session opens on the second, and says so.
func TestFailoverHappensOnlyBeforeTheSessionOpens(t *testing.T) {
	h := newHarness(t, options{})
	h.upstream.Refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+fake.Key {
			return false
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"bad key"}}`))
		return true
	}
	const requestID = contract.RequestID("req_failover")
	ws := h.open(sessionRequest(requestID, primary, secondary))
	created := read(t, ws)
	if created["deploymentId"] != string(secondary) {
		t.Fatalf("session.created = %v", created)
	}
	command(t, ws, closeCommand(requestID, "cmd_close"))
	readUntil(t, ws, "session.closed")
	report := read(t, ws)
	if report["deploymentId"] != string(secondary) || report["routeSwitches"] != float64(1) || report["outcome"] != "cancelled" {
		t.Fatalf("report = %v", report)
	}
	waitFor(t, "the cost records", func() bool { return len(h.costs.recorded()) == 2 })
	records := h.costs.recorded()
	if records[0].KeyID != "key_a" || records[0].Telemetry.Outcome != providercost.AttemptFailed || records[0].Telemetry.FailureCode != contract.CodeProviderCredentialInvalid || records[0].Served ||
		records[1].KeyID != "key_b" || !records[1].Served || records[1].AttemptIndex != 1 {
		t.Errorf("cost records = %+v", records)
	}
	if usable := h.adapter.PlatformCredentials().Projection(time.Now()).Usable; usable != 1 {
		t.Errorf("the refused key was not retired: %d usable", usable)
	}
}

// TestASessionThatNeverOpensSettlesAsFailed: every route refuses, so the
// session closes with no_route_available, empty units and a failed report.
func TestASessionThatNeverOpensSettlesAsFailed(t *testing.T) {
	h := newHarness(t, options{})
	h.upstream.Refuse = func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"busy"}}`))
		return true
	}
	ws := h.open(sessionRequest("req_no_route", primary, secondary))
	frames := readUntil(t, ws, "session.closed")
	if describe(frames) != "0:error 1:session.closed" || frames[1]["reason"] != "no_route_available" || frames[1]["deploymentId"] != nil {
		t.Fatalf("frames = %s %v", describe(frames), frames)
	}
	if units, ok := frames[1]["units"].([]any); !ok || len(units) != 0 {
		t.Errorf("units = %v; a session that never opened consumed nothing", frames[1]["units"])
	}
	report := read(t, ws)
	if report["outcome"] != "failed" || len(report["units"].([]any)) != 0 {
		t.Fatalf("report = %v", report)
	}
	expectClose(t, ws, websocket.StatusNormalClosure)
	if h.upstream.Dials() != 2 {
		t.Errorf("the session tried %d routes, want both", h.upstream.Dials())
	}
}

// TestATextAdapterIsNeverHandedASession is the routing half of the gate: a
// signed route whose deployment is served by a request adapter is refused,
// and the same session on the realtime slug opens.
func TestATextAdapterIsNeverHandedASession(t *testing.T) {
	h := newHarness(t, options{})
	ws := h.open(sessionRequest("req_text_route", textRoute))
	frames := readUntil(t, ws, "session.closed")
	failure, _ := frames[0]["error"].(map[string]any)
	if frames[0].kind() != "error" || failure["code"] != "unsupported_modality" || frames[1]["reason"] != "no_route_available" {
		t.Fatalf("frames = %v", frames)
	}
	control := h.open(sessionRequest("req_realtime_route", primary))
	if created := read(t, control); created.kind() != "session.created" {
		t.Fatalf("control = %v", created)
	}
}

// TestAProviderFailureMidSessionIsTheSessionsEnd: OpenAI reports its own
// failure and drops the connection. The session never fails over.
func TestAProviderFailureMidSessionIsTheSessionsEnd(t *testing.T) {
	h := newHarness(t, options{})
	h.upstream.Script = func(conn *fake.Conn) {
		spokenTurn(conn, "resp_1", fake.Usage(1, 1, 0, 0, 1, 1))
		conn.Send(fake.Event{"type": "error", "error": fake.Event{"type": "server_error", "message": "internal"}})
		conn.Close(websocket.StatusInternalError)
	}
	ws := h.open(sessionRequest("req_upstream_error", primary, secondary))
	frames := readUntil(t, ws, "session.closed")
	last := frames[len(frames)-3:]
	if last[0].kind() != "error" || last[0]["fatal"] != false || last[1].kind() != "error" || last[1]["fatal"] != true || last[2]["reason"] != "upstream_error" {
		t.Fatalf("frames = %s", describe(frames))
	}
	if report := read(t, ws); report["outcome"] != "partial" || report["deploymentId"] != string(primary) {
		t.Fatalf("report = %v", report)
	}
	if h.upstream.Dials() != 1 {
		t.Errorf("a session that had opened was retried on another route (%d dials)", h.upstream.Dials())
	}
	waitFor(t, "the cost record", func() bool { return len(h.costs.recorded()) == 1 })
	if record := h.costs.recorded()[0]; record.Telemetry.Outcome != providercost.AttemptFailed || record.Telemetry.FailureCode != contract.CodeProviderError {
		t.Errorf("cost record = %+v", record)
	}
}

func TestACleanUpstreamCloseEndsTheSession(t *testing.T) {
	h := newHarness(t, options{})
	h.upstream.Script = func(conn *fake.Conn) { conn.Close(websocket.StatusNormalClosure) }
	ws := h.open(sessionRequest("req_upstream_closed", primary))
	frames := readUntil(t, ws, "session.closed")
	if frames[len(frames)-1]["reason"] != "upstream_closed" {
		t.Fatalf("frames = %s", describe(frames))
	}
}

// TestShutdownClosesEverySessionAndRefusesNewOnes.
func TestShutdownClosesEverySessionAndRefusesNewOnes(t *testing.T) {
	h := newHarness(t, options{})
	ws := h.open(sessionRequest("req_shutdown", primary))
	read(t, ws)
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- h.manager.Shutdown(ctx)
	}()
	frames := readUntil(t, ws, "session.closed")
	if frames[len(frames)-1]["reason"] != "server_shutdown" {
		t.Fatalf("frames = %s", describe(frames))
	}
	if report := read(t, ws); report["requestId"] != "req_shutdown" {
		t.Fatalf("report = %v", report)
	}
	expectClose(t, ws, websocket.StatusNormalClosure)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/internal/v1/realtime", nil)
	closeBody(response)
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a draining task accepted a connection: %v", err)
	}
}

// TestAQuietConnectionIsKeptAlive: with nothing to say, Kaana still pings the
// attached connection, so a load balancer's idle timeout never drops a
// session that is merely listening.
func TestAQuietConnectionIsKeptAlive(t *testing.T) {
	h := newHarness(t, options{pingInterval: 20 * time.Millisecond})
	pings := make(chan struct{}, 64)
	request := encode(t, sessionRequest("req_keepalive", primary))
	milliseconds := time.Now().UnixMilli()
	header := http.Header{}
	header.Set(edgeauth.HeaderKeyID, h.keyID)
	header.Set(edgeauth.HeaderTimestamp, strconv.FormatInt(milliseconds, 10))
	header.Set(edgeauth.HeaderSignature, "v1="+base64.StdEncoding.EncodeToString(ed25519.Sign(h.private, edgeauth.SigningInput(h.keyID, milliseconds, request))))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+"/internal/v1/realtime", &websocket.DialOptions{
		HTTPHeader: header,
		OnPingReceived: func(context.Context, []byte) bool {
			select {
			case pings <- struct{}{}:
			default:
			}
			return true
		},
	})
	closeBody(response)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	if err := ws.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	if created := read(t, ws); created.kind() != "session.created" {
		t.Fatalf("created = %v", created)
	}
	// A ping is answered only while the client reads, as every WebSocket
	// client's is.
	go func() { _, _, _ = ws.Read(ctx) }()
	for range 3 {
		select {
		case <-pings:
		case <-ctx.Done():
			t.Fatal("a quiet connection was never pinged")
		}
	}
}

// TestACustomerCredentialIsRefusedForASession: a BYOK key is decrypted for
// one upstream call and destroyed with it; a session would hold it for up to
// an hour, a custody decision nobody has made. The route is refused, by name,
// before anything is dialled; the same session on the platform key opens.
func TestACustomerCredentialIsRefusedForASession(t *testing.T) {
	h := newHarness(t, options{})
	request := sessionRequest("req_byok", primary)
	request.AuthorizedRoutes[0].CustomerProviderCredential = &contract.CustomerProviderCredential{
		CredentialHandle: "kcred_abcdefghijklmnopqrstuvwxyz", CredentialRevision: 2,
		OwnerAccountID: "acc_realtime", ConnectionID: "pcx_exact", Environment: contract.EnvironmentProduction,
	}
	ws := h.open(request)
	frames := readUntil(t, ws, "session.closed")
	failure, _ := frames[0]["error"].(map[string]any)
	if frames[0].kind() != "error" || failure["code"] != "invalid_request" || failure["param"] != "authorizedRoutes[0].customerProviderCredential" ||
		frames[1]["reason"] != "no_route_available" {
		t.Fatalf("frames = %v", frames)
	}
	if report := read(t, ws); report["outcome"] != "failed" {
		t.Fatalf("report = %v", report)
	}
	if h.upstream.Dials() != 0 {
		t.Fatalf("a refused customer route dialled the provider %d times", h.upstream.Dials())
	}
	control := h.open(sessionRequest("req_platform", primary))
	if created := read(t, control); created.kind() != "session.created" {
		t.Fatalf("control = %v", created)
	}
}
