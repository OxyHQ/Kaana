package xaistt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	fake "github.com/OxyHQ/Kaana/internal/provider/xaistt/xaistttest"
)

const requestID contract.RequestID = "req_xai_stt_test"

func pointer[T any](value T) *T { return &value }

func route() provider.Route {
	return provider.Route{
		DeploymentID: "dep_xai_stt", Provider: Slug,
		ModelReference: "x-ai/grok-voice-transcribe-2.0@observed-2026-10-28", UpstreamModelID: "grok-voice-transcribe-2.0",
	}
}

func dictation() contract.RealtimeSessionConfig {
	return contract.RealtimeSessionConfig{
		InputAudioFormat: contract.RealtimePCM16,
		TurnDetection: contract.RealtimeTurnDetection{
			Type: contract.TurnDetectionServerVAD, CreateResponse: pointer(false), InterruptResponse: pointer(false),
		},
		InputAudioTranscription: &contract.RealtimeInputTranscription{Language: pointer("es-ES"), InterimResults: pointer(true)},
	}
}

func request(config contract.RealtimeSessionConfig) provider.RealtimeOpenRequest {
	return provider.RealtimeOpenRequest{RequestID: requestID, Route: route(), Kind: contract.RealtimeTranscription, Config: config}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newAdapter(t *testing.T, upstream *fake.Upstream, now func() time.Time) *Adapter {
	t.Helper()
	adapter, err := New(Config{Declarations: []provider.KeyDeclaration{{KeyID: "key_xai_stt", Secret: fake.Key}}, HTTPClient: upstream.Client(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func open(t *testing.T, upstream *fake.Upstream, config contract.RealtimeSessionConfig, now func() time.Time) provider.RealtimeUpstream {
	t.Helper()
	session, opened, err := newAdapter(t, upstream, now).Open(context.Background(), request(config), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.KeyID != "key_xai_stt" {
		t.Fatalf("the open attempt names key %q", opened.KeyID)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func next(t *testing.T, session provider.RealtimeUpstream) contract.RealtimeServerEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	event, err := session.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return event.Event
}

func TestItDeclaresTranscriptionSessionsOnly(t *testing.T) {
	adapter := newAdapter(t, fake.New(t), nil)
	if kinds := adapter.RealtimeSessionKinds(); !reflect.DeepEqual(kinds, []contract.RealtimeSessionKind{contract.RealtimeTranscription}) {
		t.Fatalf("kinds = %v", kinds)
	}
	var session provider.RealtimeAdapter = adapter
	if !provider.OpensRealtime(session, contract.RealtimeTranscription) || provider.OpensRealtime(session, contract.RealtimeConversation) {
		t.Fatal("the adapter must open transcription sessions and no conversation")
	}
}

func TestTheSessionIsConfiguredByItsHandshake(t *testing.T) {
	upstream := fake.New(t)
	open(t, upstream, dictation(), nil)
	queries := upstream.Queries()
	if len(queries) != 1 {
		t.Fatalf("dialled %d times", len(queries))
	}
	query := queries[0]
	want := map[string]string{
		"model": "grok-voice-transcribe-2.0", "encoding": "pcm", "sample_rate": "24000",
		"endpointing": "500", "interim_results": "true", "language": "es-ES",
	}
	for name, value := range want {
		if got := query.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if len(query) != len(want) {
		t.Errorf("the handshake carried %v, not exactly %v", query, want)
	}
}

func TestEveryContractSettingIsTranslatedOrRefused(t *testing.T) {
	t.Run("push-to-talk waits for the commit", func(t *testing.T) {
		upstream := fake.New(t)
		config := dictation()
		config.InputAudioFormat = contract.RealtimeULaw
		config.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionNone}
		config.InputAudioTranscription = &contract.RealtimeInputTranscription{}
		open(t, upstream, config, nil)
		query := upstream.Queries()[0]
		if query.Get("endpointing") != "5000" || query.Get("encoding") != "mulaw" || query.Get("sample_rate") != "8000" {
			t.Errorf("push-to-talk handshake = %v", query)
		}
		if query.Has("interim_results") || query.Has("language") {
			t.Errorf("a session asking for neither sent %v", query)
		}
	})
	t.Run("server_vad's silence and threshold", func(t *testing.T) {
		upstream := fake.New(t)
		config := dictation()
		config.TurnDetection.SilenceDurationMs = pointer(9000)
		config.TurnDetection.Threshold = pointer(0.3)
		open(t, upstream, config, nil)
		query := upstream.Queries()[0]
		if query.Get("endpointing") != "5000" || query.Get("vad_threshold") != "0.3" {
			t.Errorf("server_vad handshake = %v", query)
		}
	})
	t.Run("a language xAI does not list is detected, not sent", func(t *testing.T) {
		upstream := fake.New(t)
		config := dictation()
		config.InputAudioTranscription.Language = pointer("eu")
		open(t, upstream, config, nil)
		if upstream.Queries()[0].Has("language") {
			t.Error("a language outside xAI's table was sent")
		}
	})
	t.Run("semantic turn detection runs as server_vad", func(t *testing.T) {
		upstream := fake.New(t)
		config := dictation()
		config.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionSemanticVAD, Eagerness: pointer(contract.RealtimeVADEagerness("low")), CreateResponse: pointer(false), InterruptResponse: pointer(false)}
		open(t, upstream, config, nil)
		if got := upstream.Queries()[0].Get("endpointing"); got != "500" {
			t.Errorf("semantic_vad endpointing = %q, want Kaana's default silence", got)
		}
	})
	t.Run("a conversation is refused", func(t *testing.T) {
		upstream := fake.New(t)
		conversation := request(dictation())
		conversation.Kind = contract.RealtimeConversation
		_, _, err := newAdapter(t, upstream, nil).Open(context.Background(), conversation, nil)
		var unsupported provider.ErrUnsupported
		if !errors.As(err, &unsupported) || unsupported.Param != "kind" {
			t.Fatalf("Open = %v", err)
		}
	})
}

// TestAnUtteranceIsInterimTextThenLockedDeltasThenOneDone replays one
// utterance as xAI's guide describes it: interim text about every half second,
// a locked chunk, more interim text, and the stitched utterance final.
func TestAnUtteranceIsInterimTextThenLockedDeltasThenOneDone(t *testing.T) {
	upstream := fake.New(t)
	audio := bytes.Repeat([]byte{1, 2}, 2400)
	upstream.Script = func(conn *fake.Conn) {
		if got := conn.ExpectAudio(); !bytes.Equal(got, audio) {
			t.Errorf("xAI read %d bytes, not the %d appended", len(got), len(audio))
		}
		conn.Send(fake.Partial("hola", false, false))
		conn.Send(fake.Partial("hola qué", false, false))
		conn.Send(fake.Partial("hola qué tal", true, false))
		conn.Send(fake.Partial("cómo", false, false))
		conn.Send(fake.Partial("hola qué tal cómo estás", true, true))
		conn.ExpectControl("finalize")
		conn.Send(fake.Partial("adiós", true, true))
	}
	session := open(t, upstream, dictation(), nil)
	if err := session.Send(context.Background(), &contract.RealtimeInputAudioAppendCommand{Data: base64.StdEncoding.EncodeToString(audio)}); err != nil {
		t.Fatal(err)
	}
	type step struct {
		kind string
		item contract.RealtimeItemID
		text string
	}
	want := []step{
		{"interim", "item_1", "hola"},
		{"interim", "item_1", "hola qué"},
		{"delta", "item_1", "hola qué tal"},
		{"interim", "item_1", "hola qué tal cómo"},
		{"delta", "item_1", " cómo estás"},
		{"committed", "item_1", ""},
		{"done", "item_1", "hola qué tal cómo estás"},
	}
	read := func(count int) []step {
		var got []step
		for range count {
			switch e := next(t, session).(type) {
			case *contract.RealtimeTranscriptInterimEvent:
				got = append(got, step{"interim", e.ItemID, e.Text})
			case *contract.RealtimeTranscriptDeltaEvent:
				if e.Source != contract.RealtimeTranscriptInput {
					t.Errorf("a delta of the caller's audio has source %s", e.Source)
				}
				got = append(got, step{"delta", e.ItemID, e.Text})
			case *contract.RealtimeInputAudioCommittedEvent:
				got = append(got, step{"committed", e.ItemID, ""})
			case *contract.RealtimeTranscriptDoneEvent:
				got = append(got, step{"done", e.ItemID, e.Transcript})
			default:
				t.Fatalf("unexpected event %T", e)
			}
		}
		return got
	}
	if got := read(len(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	if err := session.Send(context.Background(), &contract.RealtimeInputAudioCommitCommand{}); err != nil {
		t.Fatal(err)
	}
	second := read(3)
	if second[0] != (step{"delta", "item_2", "adiós"}) || second[1].item != "item_2" || second[2] != (step{"done", "item_2", "adiós"}) {
		t.Fatalf("the committed utterance = %v", second)
	}
}

// TestInterimTextIsSentOnlyWhenAskedFor holds the contract: a session that did
// not ask for interim text receives none, and its deltas and done are the same.
func TestInterimTextIsSentOnlyWhenAskedFor(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Partial("hello", false, false))
		conn.Send(fake.Partial("hello there", true, true))
	}
	config := dictation()
	config.InputAudioTranscription = &contract.RealtimeInputTranscription{}
	session := open(t, upstream, config, nil)
	if delta, ok := next(t, session).(*contract.RealtimeTranscriptDeltaEvent); !ok || delta.Text != "hello there" {
		t.Fatalf("the first event was not the utterance's delta: %#v", delta)
	}
	if _, ok := next(t, session).(*contract.RealtimeInputAudioCommittedEvent); !ok {
		t.Fatal("no commit")
	}
	if done, ok := next(t, session).(*contract.RealtimeTranscriptDoneEvent); !ok || done.Transcript != "hello there" {
		t.Fatalf("done = %#v", done)
	}
}

// TestAFinalThatRevisesTheLockedTextIsTheTranscript: deltas only append, so a
// final that does not extend them is not reconciled into one: the item's
// transcript.done carries xAI's final text.
func TestAFinalThatRevisesTheLockedTextIsTheTranscript(t *testing.T) {
	upstream := fake.New(t)
	upstream.Script = func(conn *fake.Conn) {
		conn.Send(fake.Partial("I scream", true, false))
		conn.Send(fake.Partial("Ice cream, please.", true, true))
	}
	config := dictation()
	config.InputAudioTranscription = &contract.RealtimeInputTranscription{}
	session := open(t, upstream, config, nil)
	if delta := next(t, session).(*contract.RealtimeTranscriptDeltaEvent); delta.Text != "I scream" {
		t.Fatalf("delta = %q", delta.Text)
	}
	if _, ok := next(t, session).(*contract.RealtimeInputAudioCommittedEvent); !ok {
		t.Fatal("the revision was relayed as a delta")
	}
	if done := next(t, session).(*contract.RealtimeTranscriptDoneEvent); done.Transcript != "Ice cream, please." {
		t.Fatalf("done = %q", done.Transcript)
	}
}

func TestOnlyAudioAndCommitsReachXAI(t *testing.T) {
	upstream := fake.New(t)
	session := open(t, upstream, dictation(), nil)
	for _, command := range []contract.RealtimeCommand{
		&contract.RealtimeInputAudioClearCommand{},
		&contract.RealtimeSessionUpdateCommand{Config: contract.RealtimeSessionConfigUpdate{Instructions: pointer("x")}},
		&contract.RealtimeResponseCreateCommand{},
	} {
		var unsupported provider.ErrUnsupported
		if err := session.Send(context.Background(), command); !errors.As(err, &unsupported) || unsupported.Param != "type" {
			t.Errorf("%s = %v, want a refusal before anything is written", command.CommandType(), err)
		}
	}
	_ = session.Close()
	upstream.WaitClosed(t)
	if frames := upstream.Frames(); len(frames) != 0 {
		t.Errorf("a refused command reached xAI: %v", frames)
	}
}

// TestTheSessionIsMeteredByItsWallClock: from xAI's transcript.created to the
// end of the connection, rounded up once, whatever audio was sent.
func TestTheSessionIsMeteredByItsWallClock(t *testing.T) {
	upstream := fake.New(t)
	now := &clock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	session := open(t, upstream, dictation(), now.Now)
	meter, metered := session.(provider.RealtimeMeter)
	if !metered {
		t.Fatal("the session is not metered by Kaana")
	}
	now.advance(61*time.Second + 400*time.Microsecond)
	if got := meter.Measured(); !reflect.DeepEqual(got, []contract.UsageQuantity{{Unit: contract.UnitSessionMilliseconds, Quantity: 61_001}}) {
		t.Fatalf("while open: %v", got)
	}
	_ = session.Close()
	now.advance(time.Hour)
	if got := meter.Measured(); got[0].Quantity != 61_001 {
		t.Fatalf("the clock kept running after close: %v", got)
	}
}

func TestAHandshakeRefusalIsClassifiedByItsStatus(t *testing.T) {
	cases := map[string]struct {
		status  int
		code    contract.ErrorCode
		retired bool
	}{
		"invalid key":     {http.StatusUnauthorized, contract.CodeProviderCredentialInvalid, true},
		"unbilled":        {http.StatusPaymentRequired, contract.CodeProviderBillingRefused, true},
		"throttled":       {http.StatusTooManyRequests, contract.CodeRateLimited, false},
		"bad request":     {http.StatusBadRequest, contract.CodeInvalidRequest, false},
		"xAI unavailable": {http.StatusServiceUnavailable, contract.CodeProviderOverloaded, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := fake.New(t)
			upstream.Refuse = func(w http.ResponseWriter, _ *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":"refused","error":"Incorrect API key provided: ` + fake.Key + `"}`))
				return true
			}
			adapter := newAdapter(t, upstream, nil)
			_, opened, err := adapter.Open(context.Background(), request(dictation()), nil)
			var failure provider.ErrUpstream
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("Open = %v, want %s", err, tc.code)
			}
			if opened.KeyID != "key_xai_stt" {
				t.Errorf("a refused attempt must still name the key it spent, got %q", opened.KeyID)
			}
			if strings.Contains(*failure.Passthrough.Message, fake.Key) || !strings.Contains(*failure.Passthrough.Message, "[redacted]") {
				t.Errorf("the key was not redacted from xAI's message: %q", *failure.Passthrough.Message)
			}
			if retired := adapter.PlatformCredentials().Projection(time.Now()).Usable == 0; retired != tc.retired {
				t.Errorf("retired = %v, want %v", retired, tc.retired)
			}
		})
	}
}

func TestAnErrorEndsTheSession(t *testing.T) {
	t.Run("before it is ready, the open fails", func(t *testing.T) {
		upstream := fake.New(t)
		upstream.Ready = fake.Error("model unavailable")
		_, _, err := newAdapter(t, upstream, nil).Open(context.Background(), request(dictation()), nil)
		var failure provider.ErrUpstream
		if !errors.As(err, &failure) || failure.Code != contract.CodeProviderError {
			t.Fatalf("Open = %v", err)
		}
	})
	t.Run("once open, Next reports it", func(t *testing.T) {
		upstream := fake.New(t)
		upstream.Script = func(conn *fake.Conn) {
			conn.Send(fake.Error("audio frame could not be decoded"))
			conn.Close(websocket.StatusInternalError)
		}
		session := open(t, upstream, dictation(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := session.Next(ctx)
		var failure provider.ErrUpstream
		if !errors.As(err, &failure) || failure.Passthrough == nil || failure.Passthrough.Message == nil {
			t.Fatalf("Next = %v", err)
		}
	})
	t.Run("a normal close is a clean end", func(t *testing.T) {
		upstream := fake.New(t)
		upstream.Script = func(conn *fake.Conn) { conn.Close(websocket.StatusNormalClosure) }
		session := open(t, upstream, dictation(), nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := session.Next(ctx); !errors.Is(err, provider.ErrRealtimeUpstreamClosed) {
			t.Fatalf("Next = %v", err)
		}
	})
}

// TestTheStatementServesWhatTheAdapterOpens: the public gate refuses, before
// any grant, what the published statement says cannot be served
// (RealtimeParameters.Unservable); a dictation open passes it, semantic turn
// detection too (it runs as server_vad), and an output cap does not.
func TestTheStatementServesWhatTheAdapterOpens(t *testing.T) {
	statement := newAdapter(t, fake.New(t), nil).statement
	if issues := statement.Parameters.Unservable("config", dictation().Fields()); len(issues) != 0 {
		t.Fatalf("a dictation open is unservable: %v", issues)
	}
	semantic := dictation()
	semantic.TurnDetection = contract.RealtimeTurnDetection{Type: contract.TurnDetectionSemanticVAD, CreateResponse: pointer(false), InterruptResponse: pointer(false)}
	if issues := statement.Parameters.Unservable("config", semantic.Fields()); len(issues) != 0 {
		t.Fatalf("semantic turn detection, which runs as server_vad, is unservable: %v", issues)
	}
	responding := dictation()
	responding.MaxOutputTokens = pointer(100)
	if issues := statement.Parameters.Unservable("config", responding.Fields()); len(issues) == 0 {
		t.Fatal("control: an output cap passed a statement that never responds")
	}
}
