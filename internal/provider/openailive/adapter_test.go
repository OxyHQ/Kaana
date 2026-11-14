package openailive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/openailive/openailivetest"
)

func pointer[T any](value T) *T { return &value }

func liveConfig(format contract.RealtimeAudioFormat, acknowledgement contract.RealtimeDelegationAcknowledgement) contract.RealtimeSessionConfig {
	return contract.RealtimeSessionConfig{
		Instructions: pointer("You are Alia. Speak Spanish."), Voice: pointer("marin"),
		InputAudioFormat: format, OutputAudioFormat: pointer(format),
		TurnDetection: contract.RealtimeTurnDetection{Type: contract.TurnDetectionServerVAD, Threshold: pointer(0.7), SilenceDurationMs: pointer(300),
			CreateResponse: pointer(true), InterruptResponse: pointer(true)},
		InputAudioTranscription: &contract.RealtimeInputTranscription{Language: pointer("es-ES")},
		Delegation:              &contract.RealtimeDelegationConfig{Acknowledgement: acknowledgement},
	}
}

func openRequest(config contract.RealtimeSessionConfig) provider.RealtimeOpenRequest {
	return provider.RealtimeOpenRequest{
		RequestID: "req_live",
		Route:     provider.Route{DeploymentID: "dep_live", Provider: Slug, UpstreamModelID: "gpt-live-1"},
		Kind:      contract.RealtimeLive,
		Config:    config,
	}
}

// clock is a settable clock for the wall-clock fallback.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

func newAdapter(t *testing.T, upstream *fake.Upstream, now func() time.Time, keys ...string) *Adapter {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{fake.Key}
	}
	declarations := make([]provider.KeyDeclaration, 0, len(keys))
	for index, key := range keys {
		declarations = append(declarations, provider.KeyDeclaration{KeyID: []string{"key_a", "key_b"}[index], Secret: key})
	}
	adapter, err := New(Config{Declarations: declarations, HTTPClient: upstream.Client(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func open(t *testing.T, adapter *Adapter, config contract.RealtimeSessionConfig) *session {
	t.Helper()
	upstream, opened, err := adapter.Open(context.Background(), openRequest(config), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened.KeyID == "" {
		t.Fatal("the open did not name the key it spent")
	}
	t.Cleanup(func() { _ = upstream.Close() })
	return upstream.(*session)
}

// next reads the session's next event, bounded.
func next(t *testing.T, s *session) (provider.RealtimeUpstreamEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Next(ctx)
}

func nextEvent(t *testing.T, s *session) contract.RealtimeServerEvent {
	t.Helper()
	event, err := next(t, s)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return event.Event
}

// TestTheSessionStartIsTheWholeSession: one session.start carries the model,
// the live preamble before the persona, one audio format both ways in GPT-Live's
// spelling, the voice and client delegation with the acknowledgement delay, and
// store false; nothing a live voice does not take (turn detection, thresholds,
// a transcription language) is sent. Audio is sent only after session.started.
func TestTheSessionStartIsTheWholeSession(t *testing.T) {
	formats := map[contract.RealtimeAudioFormat]fake.Event{
		contract.RealtimePCM16: {"type": "audio/pcm", "rate": float64(24000)},
		contract.RealtimeULaw:  {"type": "audio/pcmu", "rate": float64(8000)},
		contract.RealtimeALaw:  {"type": "audio/pcma", "rate": float64(8000)},
	}
	delays := map[contract.RealtimeDelegationAcknowledgement]float64{contract.RealtimeAcknowledgementAuto: 400, contract.RealtimeAcknowledgementNone: 0}
	for format, wire := range formats {
		for acknowledgement, delay := range delays {
			upstream := fake.New(t)
			s := open(t, newAdapter(t, upstream, nil), liveConfig(format, acknowledgement))
			conn := upstream.Accept(t)
			start := upstream.Received()[0]
			want := fake.Event{
				"type": "session.start", "event_id": startEventID,
				"session": map[string]any{
					"model":        "gpt-live-1",
					"instructions": provider.LivePreamble + "\n\nYou are Alia. Speak Spanish.",
					"audio":        map[string]any{"format": map[string]any(wire), "output": map[string]any{"voice": "marin"}},
					"delegation":   map[string]any{"type": "client", "acknowledgement_delay_ms": delay},
					"store":        false,
				},
			}
			if !reflect.DeepEqual(start, want) {
				t.Errorf("%s/%s: session.start = %v\nwant %v", format, acknowledgement, start, want)
			}
			if err := s.Send(context.Background(), &contract.RealtimeInputAudioAppendCommand{CommandID: "cmd_audio", Data: "AAAAAA=="}); err != nil {
				t.Fatal(err)
			}
			if got := conn.Expect("session.input_audio.append"); got["audio"] != "AAAAAA==" || got["event_id"] != "cmd_audio" {
				t.Errorf("audio append = %v", got)
			}
		}
	}
}

// TestWhatGPTLiveCannotHoldIsRefusedBeforeAnythingIsDialled, with an open
// that dials as the positive control.
func TestWhatGPTLiveCannotHoldIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	upstream := fake.New(t)
	adapter := newAdapter(t, upstream, nil)
	cases := map[string]func(*provider.RealtimeOpenRequest){
		"a conversation": func(r *provider.RealtimeOpenRequest) { r.Kind = contract.RealtimeConversation },
		"a voice GPT-Live does not have": func(r *provider.RealtimeOpenRequest) {
			r.Config.Voice = pointer("eve")
		},
		"another provider's route": func(r *provider.RealtimeOpenRequest) { r.Route.Provider = "openai-realtime" },
		"no upstream model":        func(r *provider.RealtimeOpenRequest) { r.Route.UpstreamModelID = "" },
	}
	for name, mutate := range cases {
		request := openRequest(liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
		mutate(&request)
		if _, _, err := adapter.Open(context.Background(), request, nil); !errors.As(err, new(provider.ErrUnsupported)) {
			t.Errorf("%s: open = %v, want a refusal", name, err)
		}
	}
	if upstream.Dials() != 0 {
		t.Fatalf("a refused session reached OpenAI %d times", upstream.Dials())
	}
	open(t, adapter, liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
	if upstream.Dials() != 1 {
		t.Fatalf("the control open dialled %d times", upstream.Dials())
	}
}

// TestARefusedHandshakeIsClassifiedAndTheNextKeyOpens: a 401 retires the
// first key and the second opens; an account OpenAI cannot bill is a billing
// refusal; the key the body echoes never leaves the adapter.
func TestARefusedHandshakeIsClassifiedAndTheNextKeyOpens(t *testing.T) {
	upstream := fake.New(t)
	upstream.Refuse = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+fake.Key {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided: ` + fake.Key + `"}}`))
		return true
	}
	adapter := newAdapter(t, upstream, nil, fake.Key, fake.SecondKey)
	_, opened, err := adapter.Open(context.Background(), openRequest(liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto)), nil)
	if err != nil || opened.KeyID != "key_b" {
		t.Fatalf("open = %v on %q; the second key should have opened", err, opened.KeyID)
	}
	if usable := adapter.PlatformCredentials().Projection(time.Now()).Usable; usable != 1 {
		t.Fatalf("usable keys = %d; the refused key is retired", usable)
	}

	for name, refusal := range map[string]struct {
		status int
		body   string
		code   contract.ErrorCode
	}{
		"unauthorized":   {http.StatusUnauthorized, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided: ` + fake.Key + `"}}`, contract.CodeProviderCredentialInvalid},
		"quota":          {http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota"}}`, contract.CodeProviderBillingRefused},
		"throttle":       {http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down"}}`, contract.CodeRateLimited},
		"unknown model":  {http.StatusNotFound, `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"no such model"}}`, contract.CodeModelNotFound},
		"overloaded":     {http.StatusServiceUnavailable, `{}`, contract.CodeProviderOverloaded},
		"internal error": {http.StatusInternalServerError, `{}`, contract.CodeProviderError},
	} {
		refused := fake.New(t)
		refused.Refuse = func(w http.ResponseWriter, _ *http.Request) bool {
			w.WriteHeader(refusal.status)
			_, _ = w.Write([]byte(refusal.body))
			return true
		}
		_, _, err := newAdapter(t, refused, nil).Open(context.Background(), openRequest(liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto)), nil)
		var failure provider.ErrUpstream
		if !errors.As(err, &failure) || failure.Code != refusal.code {
			t.Errorf("%s: open = %v, want %s", name, err, refusal.code)
			continue
		}
		// Positive control for the redaction: the body really echoed the key.
		if strings.Contains(refusal.body, fake.Key) && (failure.Passthrough == nil || failure.Passthrough.Message == nil ||
			strings.Contains(*failure.Passthrough.Message, fake.Key) || strings.Contains(err.Error(), fake.Key)) {
			t.Errorf("%s: the key OpenAI echoed was not redacted: %+v", name, failure.Passthrough)
		}
	}
}

// TestAnErrorAnsweringTheStartIsOpenAIsVerdict: an error before
// session.started is classified like a refused request and the session never
// opened.
func TestAnErrorAnsweringTheStartIsOpenAIsVerdict(t *testing.T) {
	upstream := fake.New(t)
	upstream.Start = func(fake.Event) fake.Event {
		return fake.Event{"type": "error", "event_id": "event_error", "error": fake.Event{"type": "invalid_request_error", "code": "unknown_parameter",
			"message": "Unknown parameter", "param": "session.voice", "client_event_id": startEventID}}
	}
	_, _, err := newAdapter(t, upstream, nil).Open(context.Background(), openRequest(liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto)), nil)
	var failure provider.ErrUpstream
	if !errors.As(err, &failure) || failure.Code != contract.CodeInvalidRequest {
		t.Fatalf("open = %v, want OpenAI's invalid_request", err)
	}
}

// TestTheTurnIsHandedOverWithThePersonsWords: the input transcript is relayed
// as it arrives, one item per turn, and each delegation ends that item with its
// transcript.done and hands its words over; the next turn is a new item.
func TestTheTurnIsHandedOverWithThePersonsWords(t *testing.T) {
	upstream := fake.New(t)
	s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
	conn := upstream.Accept(t)
	conn.Hear("Book a table ", 100, 900)
	conn.Hear("for ⟦two⟧.", 900, 1400)
	conn.Delegate("del_1", 1500)
	conn.Hear("No, three.", 4000, 4800)
	conn.Delegate("del_2", 5000)

	want := []contract.RealtimeServerEvent{
		&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: "input_1", Text: "Book a table "},
		&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: "input_1", Text: "for two."},
		&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptInput, ItemID: "input_1", Transcript: "Book a table for two."},
		&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_1", Text: "Book a table for two."},
		&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptInput, ItemID: "input_2", Text: "No, three."},
		&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptInput, ItemID: "input_2", Transcript: "No, three."},
		&contract.RealtimeDelegationRequestedEvent{DelegationID: "del_2", Text: "No, three."},
	}
	for index, expected := range want {
		if got := nextEvent(t, s); !reflect.DeepEqual(got, expected) {
			t.Fatalf("event %d = %#v, want %#v", index, got, expected)
		}
	}
	// A session that asked for the acknowledgement sends nothing for it.
	if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.start"}) {
		t.Errorf("the provider received %v", types)
	}
}

// TestNoAcknowledgementSuppressesEachOne: a session that asked for none
// answers every delegation with an empty thinking append naming it.
func TestNoAcknowledgementSuppressesEachOne(t *testing.T) {
	upstream := fake.New(t)
	s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementNone))
	conn := upstream.Accept(t)
	conn.Delegate("del_1", 100)
	if requested, ok := nextEvent(t, s).(*contract.RealtimeDelegationRequestedEvent); !ok || requested.DelegationID != "del_1" || requested.Text != "" {
		t.Fatalf("requested = %#v", requested)
	}
	if got := conn.Expect("session.thinking.append"); got["delegation_id"] != "del_1" || got["content"] != "" {
		t.Fatalf("suppression = %v", got)
	}
}

// TestTheVoiceIsUtteranceByUtterance: the voice's transcript is relayed as it
// arrives, ⟦ ⟧ stripped; an utterance ends at a gap of the turn gap on
// OpenAI's own clock, and after the same gap of silence on Kaana's; its audio
// is split into contract frames under the same response.
func TestTheVoiceIsUtteranceByUtterance(t *testing.T) {
	upstream := fake.New(t)
	adapter := newAdapter(t, upstream, nil)
	adapter.outputGap = 300 * time.Millisecond
	s := open(t, adapter, liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
	conn := upstream.Accept(t)

	audio := make([]byte, provider.MaxAudioChunkBytes+10)
	for index := range audio {
		audio[index] = byte(index)
	}
	conn.Speak(audio)
	conn.Say("It is ", 1000, 1200)
	conn.Say("⟦19:30⟧.", 1200, 1600)
	// 300 ms after the last fragment ended on OpenAI's clock: a new utterance.
	conn.Say("Anything else?", 1900, 2400)

	response1, response2 := contract.RealtimeResponseID("response_1"), contract.RealtimeResponseID("response_2")
	first, second := nextEvent(t, s), nextEvent(t, s)
	frames := []*contract.RealtimeOutputAudioDeltaEvent{first.(*contract.RealtimeOutputAudioDeltaEvent), second.(*contract.RealtimeOutputAudioDeltaEvent)}
	var joined []byte
	for _, frame := range frames {
		decoded, _ := base64.StdEncoding.DecodeString(frame.Data)
		if len(decoded) > provider.MaxAudioChunkBytes || frame.ResponseID != response1 || frame.ItemID != "output_1" || frame.Format != contract.RealtimePCM16 {
			t.Fatalf("frame = %+v (%d bytes)", frame, len(decoded))
		}
		joined = append(joined, decoded...)
	}
	if !reflect.DeepEqual(joined, audio) {
		t.Fatal("the frames do not concatenate to the audio OpenAI sent")
	}
	want := []contract.RealtimeServerEvent{
		&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptOutput, ItemID: "output_1", ResponseID: &response1, Text: "It is "},
		&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptOutput, ItemID: "output_1", ResponseID: &response1, Text: "19:30."},
		&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptOutput, ItemID: "output_1", ResponseID: &response1, Transcript: "It is 19:30."},
		&contract.RealtimeTranscriptDeltaEvent{Source: contract.RealtimeTranscriptOutput, ItemID: "output_2", ResponseID: &response2, Text: "Anything else?"},
		// Silence on Kaana's clock ends the last one.
		&contract.RealtimeTranscriptDoneEvent{Source: contract.RealtimeTranscriptOutput, ItemID: "output_2", ResponseID: &response2, Transcript: "Anything else?"},
	}
	for index, expected := range want {
		if got := nextEvent(t, s); !reflect.DeepEqual(got, expected) {
			t.Fatalf("event %d = %#v, want %#v", index, got, expected)
		}
	}
}

// TestTheDelegationCommandsAreGPTLivesAppends: a reply is spoken through
// commentary appends cut to GPT-Live's bound on sentence boundaries, each
// verbatim span wrapped and whole; context is a thinking append; a cancel is an
// instructions append naming the turn.
func TestTheDelegationCommandsAreGPTLivesAppends(t *testing.T) {
	upstream := fake.New(t)
	s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
	conn := upstream.Accept(t)
	conn.Delegate("del_1", 100)
	nextEvent(t, s)

	sentence := "Your table is booked at the restaurant on the corner of the square, by the window you asked for. "
	address := "Calle de Alcalá 42, 28014 Madrid"
	text := strings.Repeat(sentence, 4) + "The address is " + address + ". " + strings.Repeat(sentence, 3)
	if len(text) <= maxAppendBytes {
		t.Fatalf("the reply (%d bytes) must exceed one append for this test to mean anything", len(text))
	}
	if err := s.Send(context.Background(), &contract.RealtimeDelegationReplyCommand{CommandID: "cmd_reply", DelegationID: "del_1", Text: text, Verbatim: []string{address}, Final: true}); err != nil {
		t.Fatal(err)
	}
	var spoken []string
	for len(strings.Join(spoken, " ")) < len(provider.WrapVerbatim(text, []string{address}))-1 {
		event := conn.Expect("session.commentary.append")
		content, _ := event["content"].(string)
		if event["delegation_id"] != "del_1" || len(content) > maxAppendBytes || len(content) == 0 {
			t.Fatalf("append = %v (%d bytes)", event, len(content))
		}
		if !strings.HasSuffix(content, ".") {
			t.Errorf("append %q is not cut on a sentence boundary", content)
		}
		spoken = append(spoken, content)
	}
	if got, want := strings.Join(spoken, " "), strings.TrimSpace(provider.WrapVerbatim(text, []string{address})); got != want {
		t.Fatalf("the appends say %q, want %q", got, want)
	}
	if !strings.Contains(strings.Join(spoken, "\x00"), provider.VerbatimOpen+address+provider.VerbatimClose) {
		t.Fatal("the verbatim span was not one append's, wrapped")
	}

	if err := s.Send(context.Background(), &contract.RealtimeDelegationContextCommand{CommandID: "cmd_context", DelegationID: "del_1", Text: "still ⟦checking⟧"}); err != nil {
		t.Fatal(err)
	}
	if got := conn.Expect("session.thinking.append"); got["content"] != "still checking" || got["delegation_id"] != "del_1" || got["event_id"] != "cmd_context" {
		t.Fatalf("context = %v", got)
	}
	if err := s.Send(context.Background(), &contract.RealtimeDelegationCancelCommand{CommandID: "cmd_cancel", DelegationID: "del_1"}); err != nil {
		t.Fatal(err)
	}
	if got := conn.Expect("session.instructions.append"); got["content"] != cancelInstruction || got["delegation_id"] != "del_1" {
		t.Fatalf("cancel = %v", got)
	}
}

// TestWhatTheVoiceDoesNotTakeIsRefusedUnwritten: commands of another kind and
// audio that is not whole samples are refused before anything is written; an
// append is the positive control.
func TestWhatTheVoiceDoesNotTakeIsRefusedUnwritten(t *testing.T) {
	upstream := fake.New(t)
	s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
	conn := upstream.Accept(t)
	for _, command := range []contract.RealtimeCommand{
		&contract.RealtimeResponseCreateCommand{CommandID: "cmd_response"},
		&contract.RealtimeResponseCancelCommand{CommandID: "cmd_cancel"},
		&contract.RealtimeItemDeleteCommand{CommandID: "cmd_delete", ItemID: "item"},
		&contract.RealtimeInputAudioAppendCommand{CommandID: "cmd_odd", Data: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
		&contract.RealtimeInputAudioAppendCommand{CommandID: "cmd_garbage", Data: "not base64!"},
	} {
		if err := s.Send(context.Background(), command); !errors.As(err, new(provider.ErrUnsupported)) {
			t.Errorf("%s: send = %v, want a refusal", command.CommandType(), err)
		}
	}
	if err := s.Send(context.Background(), &contract.RealtimeInputAudioAppendCommand{CommandID: "cmd_even", Data: base64.StdEncoding.EncodeToString([]byte{1, 2})}); err != nil {
		t.Fatal(err)
	}
	conn.Expect("session.input_audio.append")
	if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.start", "session.input_audio.append"}) {
		t.Fatalf("the provider received %v", types)
	}
}

// TestAnInBandErrorNamesTheCommandItRefuses, a later chunk's included, and a
// failure about the session itself is how the session ends.
func TestAnInBandErrorNamesTheCommandItRefuses(t *testing.T) {
	upstream := fake.New(t)
	s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
	conn := upstream.Accept(t)
	conn.Delegate("del_1", 100)
	nextEvent(t, s)
	long := strings.Repeat("A sentence that is long enough to need cutting up. ", 20)
	if err := s.Send(context.Background(), &contract.RealtimeDelegationReplyCommand{CommandID: "cmd_reply", DelegationID: "del_1", Text: long, Final: true}); err != nil {
		t.Fatal(err)
	}
	conn.Expect("session.commentary.append")
	conn.Expect("session.commentary.append")
	conn.Error("invalid_request_error", "content_too_long", "cmd_reply#1")
	event, err := next(t, s)
	if err != nil || event.CommandID == nil || *event.CommandID != "cmd_reply" {
		t.Fatalf("error event = %+v, %v; want it to name cmd_reply", event, err)
	}
	if failure := event.Event.(*contract.RealtimeErrorEvent); failure.Fatal || failure.Error.Code != contract.CodeInvalidRequest {
		t.Fatalf("error = %+v", failure)
	}
	conn.Error("server_error", "", "")
	if failure := nextEvent(t, s).(*contract.RealtimeErrorEvent); failure.Error.Code != contract.CodeProviderError {
		t.Fatalf("server error = %+v", failure)
	}
	conn.Drop()
	for {
		if _, err := next(t, s); err != nil {
			var upstreamFailure provider.ErrUpstream
			if !errors.As(err, &upstreamFailure) || upstreamFailure.Code != contract.CodeProviderError || errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
				t.Fatalf("the session ended with %v, want OpenAI's reported failure", err)
			}
			return
		}
	}
}

// drain reads every event up to the session's end.
func drain(t *testing.T, s *session) ([]contract.RealtimeServerEvent, error) {
	t.Helper()
	var events []contract.RealtimeServerEvent
	for {
		event, err := next(t, s)
		if err != nil {
			return events, err
		}
		events = append(events, event.Event)
	}
}

// TestTheSessionSettlesOpenAIsSecondsOrKaanasClock: a session Kaana ends asks
// OpenAI to close and settles session.closed's seconds, provider_reported and
// rounded up once; one OpenAI ends is settled from its own session.closed
// without a close; one whose session.closed never comes settles Kaana's wall
// clock from the handshake, oxy_measured.
func TestTheSessionSettlesOpenAIsSecondsOrKaanasClock(t *testing.T) {
	t.Run("Kaana closes", func(t *testing.T) {
		upstream := fake.New(t)
		s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
		conn := upstream.Accept(t)
		conn.Usage("3.5")
		conn.Bill("12.3001")
		units, source := s.FinalUsage(context.Background())
		if !reflect.DeepEqual(units, []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 12_301}}) || source != contract.UsageProviderReported {
			t.Fatalf("final usage = %v %s", units, source)
		}
		if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.start", "session.close"}) {
			t.Fatalf("the provider received %v", types)
		}
		if _, err := drain(t, s); !errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
			t.Fatalf("the session ended with %v", err)
		}
	})
	t.Run("OpenAI ends it", func(t *testing.T) {
		upstream := fake.New(t)
		s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
		conn := upstream.Accept(t)
		conn.Say("Goodbye.", 100, 500)
		conn.Closed("expired", "3600")
		events, err := drain(t, s)
		if !errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
			t.Fatalf("the session ended with %v", err)
		}
		if done, ok := events[len(events)-1].(*contract.RealtimeTranscriptDoneEvent); !ok || done.Transcript != "Goodbye." {
			t.Fatalf("the voice's last utterance was not finished: %#v", events)
		}
		units, source := s.FinalUsage(context.Background())
		if !reflect.DeepEqual(units, []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 3_600_000}}) || source != contract.UsageProviderReported {
			t.Fatalf("final usage = %v %s", units, source)
		}
		if types := upstream.ReceivedTypes(); !reflect.DeepEqual(types, []string{"session.start"}) {
			t.Fatalf("a session OpenAI closed was asked to close: %v", types)
		}
	})
	t.Run("a safety filter ends it", func(t *testing.T) {
		upstream := fake.New(t)
		s := open(t, newAdapter(t, upstream, nil), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
		upstream.Accept(t).Closed("content", "2")
		_, err := drain(t, s)
		var failure provider.ErrUpstream
		if !errors.As(err, &failure) || failure.Code != contract.CodeUpstreamContentFiltered {
			t.Fatalf("the session ended with %v", err)
		}
		if units, source := s.FinalUsage(context.Background()); units[0].Quantity != 2_000 || source != contract.UsageProviderReported {
			t.Fatalf("final usage = %v %s", units, source)
		}
	})
	t.Run("no session.closed", func(t *testing.T) {
		upstream := fake.New(t)
		now := &clock{now: time.Unix(1_800_000_000, 0)}
		s := open(t, newAdapter(t, upstream, now.read), liveConfig(contract.RealtimePCM16, contract.RealtimeAcknowledgementAuto))
		conn := upstream.Accept(t)
		conn.WithholdClosed()
		now.advance(61*time.Second + 400*time.Microsecond)
		units, source := s.FinalUsage(context.Background())
		if !reflect.DeepEqual(units, []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 61_001}}) || source != contract.UsageOxyMeasured {
			t.Fatalf("final usage = %v %s", units, source)
		}
	})
}

func TestSplitAppend(t *testing.T) {
	span := provider.VerbatimOpen + strings.Repeat("Ω", 300) + provider.VerbatimClose
	cases := map[string]struct {
		text string
		want []string
	}{
		"short": {"Hello there.", []string{"Hello there."}},
		"empty": {"   ", nil},
		"sentences": {strings.Repeat("a", 300) + ". " + strings.Repeat("b", 300) + ".",
			[]string{strings.Repeat("a", 300) + ".", strings.Repeat("b", 300) + "."}},
		"no sentence end, words": {strings.Repeat("word ", 120),
			[]string{strings.TrimSpace(strings.Repeat("word ", 100)), strings.TrimSpace(strings.Repeat("word ", 20))}},
		"CJK": {strings.Repeat("今天天气很好。", 30),
			[]string{strings.Repeat("今天天气很好。", 23), strings.Repeat("今天天气很好。", 7)}},
		"a span past the bound is whole": {span + " and more.", []string{span, "and more."}},
		"a span is never cut": {strings.Repeat("x", 470) + " " + provider.VerbatimOpen + "1234567890 ABCDEFGHIJ 1234567890" + provider.VerbatimClose + ".",
			[]string{strings.Repeat("x", 470), provider.VerbatimOpen + "1234567890 ABCDEFGHIJ 1234567890" + provider.VerbatimClose + "."}},
	}
	for name, test := range cases {
		got := splitAppend(test.text)
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: %d parts %q, want %d parts", name, len(got), got, len(test.want))
		}
		for _, part := range got {
			if !utf8.ValidString(part) || len(part) > maxAppendBytes && !strings.HasPrefix(part, provider.VerbatimOpen) {
				t.Errorf("%s: part of %d bytes", name, len(part))
			}
		}
	}
}

func TestCeilMilliseconds(t *testing.T) {
	for seconds, want := range map[string]int{"0": 0, "12.3": 12_300, "12.3001": 12_301, "0.0001": 1, "3600": 3_600_000, "1e1": 10_000} {
		if got, ok := ceilMilliseconds(json.Number(seconds)); !ok || got != want {
			t.Errorf("%s s = %d ms (%t), want %d", seconds, got, ok, want)
		}
	}
	for _, invalid := range []string{"-1", "NaN", ""} {
		if _, ok := ceilMilliseconds(json.Number(invalid)); ok {
			t.Errorf("%q was read as seconds", invalid)
		}
	}
}
