package openailive

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// The GPT-Live client and server events this adapter writes and reads, spelled
// as https://developers.openai.com/api/reference/resources/live/primary-websocket
// spells them (read 2026-11-14).

type clientEvent struct {
	Type    string        `json:"type"`
	EventID string        `json:"event_id,omitempty"`
	Session *sessionStart `json:"session,omitempty"`
	// Audio is session.input_audio.append's base64 audio.
	Audio string `json:"audio,omitempty"`
	// DelegationID and Content are the three appends'. delegation_id is
	// required (string or null) and content is a plain string, so neither is
	// omitted: an empty thinking append is how an automatic acknowledgement
	// is suppressed.
	DelegationID *string `json:"delegation_id,omitempty"`
	Content      *string `json:"content,omitempty"`
}

// sessionStart is `session.start.session`. Everything in it is immutable once
// the session has started.
type sessionStart struct {
	Model        string     `json:"model"`
	Instructions string     `json:"instructions"`
	Audio        audio      `json:"audio"`
	Delegation   delegation `json:"delegation"`
	// Store is sent false: GPT-Live's default, stated so that no recording of
	// the caller is kept for forking ("Stored recordings: available for 30
	// days"), whatever the account's default becomes.
	Store bool `json:"store"`
}

type audio struct {
	Format audioFormat `json:"format"`
	Output audioOutput `json:"output"`
}

type audioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

type audioOutput struct {
	Voice string `json:"voice"`
}

type delegation struct {
	Type                   string `json:"type"`
	AcknowledgementDelayMs int    `json:"acknowledgement_delay_ms"`
}

type serverEvent struct {
	Type          string `json:"type"`
	EventID       string `json:"event_id"`
	ClientEventID string `json:"client_event_id"`
	// Delta is output audio (base64) or a transcript fragment.
	Delta string `json:"delta"`
	// StartMs and EndMs are a transcript fragment's span, in milliseconds from
	// the session's start, end exclusive. Output audio on the primary
	// WebSocket carries neither.
	StartMs    *int64           `json:"start_ms"`
	EndMs      *int64           `json:"end_ms"`
	OffsetMs   *int64           `json:"offset_ms"`
	Delegation *delegationEvent `json:"delegation"`
	Usage      *usage           `json:"usage"`
	Reason     string           `json:"reason"`
	Error      *wireError       `json:"error"`
}

type delegationEvent struct {
	ID     string `json:"id"`
	Target string `json:"target"`
}

// usage is `session.usage.updated.usage` and `session.closed.usage`: the
// voice session's seconds, a number, cumulative ("do not sum across events").
type usage struct {
	Seconds *json.Number `json:"seconds"`
}

type wireError struct {
	Type          string  `json:"type"`
	Code          *string `json:"code"`
	Message       string  `json:"message"`
	Param         *string `json:"param"`
	ClientEventID *string `json:"client_event_id"`
}

func (e wireError) code() string {
	if e.Code == nil {
		return ""
	}
	return *e.Code
}

// acknowledgementDelayMs is how long GPT-Live waits before it says a short
// "one moment" for a turn it handed over (Kaana ADR 0012 §2): `auto` 400 ms,
// `none` 0, where every delegation is also answered with an empty
// session.thinking.append, which cancels the pending acknowledgement
// (live-delegation guide, "Automatic acknowledgments"). UNVERIFIED: whether a
// delay of 0 requests the acknowledgement at once or none at all; the empty
// append covers either.
var acknowledgementDelayMs = map[contract.RealtimeDelegationAcknowledgement]int{
	contract.RealtimeAcknowledgementAuto: 400,
	contract.RealtimeAcknowledgementNone: 0,
}

// startEvent is the whole session: the model the route names, the live
// preamble before the caller's persona, one audio format both ways, the voice
// and client delegation. A live session's turn detection, input transcription
// language and thresholds are not sent: GPT-Live detects the turns itself and
// documents no setting for either.
func startEvent(request provider.RealtimeOpenRequest) clientEvent {
	config := request.Config
	format, _ := wireFormat(config.InputAudioFormat)
	return clientEvent{Type: "session.start", EventID: startEventID, Session: &sessionStart{
		Model:        request.Route.UpstreamModelID,
		Instructions: provider.LiveInstructions(config.Instructions),
		Audio:        audio{Format: format, Output: audioOutput{Voice: *config.Voice}},
		Delegation:   delegation{Type: "client", AcknowledgementDelayMs: acknowledgementDelayMs[config.Delegation.Acknowledgement]},
	}}
}

// wireFormat names an encoding the way GPT-Live does: one format both ways,
// `audio/pcm` at 24 000 Hz (mono 16-bit little-endian), or G.711 at 8 000 Hz.
func wireFormat(format contract.RealtimeAudioFormat) (audioFormat, error) {
	switch format {
	case contract.RealtimePCM16:
		return audioFormat{Type: "audio/pcm", Rate: 24_000}, nil
	case contract.RealtimeULaw:
		return audioFormat{Type: "audio/pcmu", Rate: 8_000}, nil
	case contract.RealtimeALaw:
		return audioFormat{Type: "audio/pcma", Rate: 8_000}, nil
	}
	return audioFormat{}, refused("config.inputAudioFormat", "this model cannot carry "+string(format)+" audio")
}

func refused(param, detail string) error {
	return provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: param, Detail: detail}
}

// maxAppendBytes bounds one session.commentary/thinking/instructions.append.
// GPT-Live bounds `content` at 500 tokens, and no tokenizer is documented for
// it. A token is never shorter than one byte of UTF-8 in a byte-level
// vocabulary, so 500 bytes is at most 500 tokens whatever the language: a
// bound that is certain, where a characters-per-token estimate is not.
const maxAppendBytes = 500

// splitAppend cuts text into the appends GPT-Live takes: each at most
// maxAppendBytes, cut at the last sentence end that fits, else the last space,
// else the last character, and never inside a verbatim span. A span longer
// than the bound is one append of its own, whole: a span is never cut (Kaana
// ADR 0012 §2), and at 256 UTF-16 units it is, in every script, far under 500
// tokens. Whitespace at a cut is dropped; nothing else is.
func splitAppend(text string) []string {
	var parts []string
	for text = strings.TrimSpace(text); text != ""; text = strings.TrimSpace(text) {
		if len(text) <= maxAppendBytes {
			return append(parts, text)
		}
		cut := cutPoint(text)
		parts = append(parts, strings.TrimSpace(text[:cut]))
		text = text[cut:]
	}
	return parts
}

// cutPoint is where the first append of text ends: the best boundary at or
// before maxAppendBytes outside a verbatim span, or, when a span covers the
// whole bound, the end of that span.
func cutPoint(text string) int {
	sentence, space, character := 0, 0, 0
	inSpan := false
	for index, r := range text {
		if index > maxAppendBytes {
			break
		}
		if !inSpan && index > 0 {
			character = index
			previous, _ := utf8.DecodeLastRuneInString(text[:index])
			switch {
			case unicode.IsSpace(r) && sentenceEnd(text[:index]),
				// A CJK sentence ends at its full-width mark, with no space.
				strings.ContainsRune("。！？", previous):
				sentence = index
			case unicode.IsSpace(r) && !unicode.IsSpace(previous):
				space = index
			}
		}
		switch string(r) {
		case provider.VerbatimOpen:
			inSpan = true
		case provider.VerbatimClose:
			inSpan = false
		}
	}
	for _, cut := range []int{sentence, space, character} {
		if cut > 0 {
			return cut
		}
	}
	// A span starts the text and runs past the bound: it ends the append.
	if end := strings.Index(text, provider.VerbatimClose); end >= 0 {
		return end + len(provider.VerbatimClose)
	}
	return len(text)
}

// sentenceEnd reports text that ends a sentence: its last character, past any
// closing quote, bracket or verbatim mark, is a full stop, question or
// exclamation mark, an ellipsis, or one of their CJK forms.
func sentenceEnd(text string) bool {
	trimmed := strings.TrimRightFunc(text, func(r rune) bool {
		return strings.ContainsRune(`"')]}»”’`+provider.VerbatimClose, r)
	})
	last, _ := utf8.DecodeLastRuneInString(trimmed)
	return strings.ContainsRune(".!?…。！？", last)
}

// cancelInstruction is what GPT-Live is told when the client drops a turn:
// an instructions append "can interrupt current speech" (live-delegation
// guide), and it names the delegation so the voice drops that turn only.
const cancelInstruction = "The person's request for this delegation has been dropped. Stop speaking about it, say nothing more about it, and do not answer it."
