package provider

import "strings"

// What every adapter of a `live` session shares (Kaana ADR 0012 §1, §2): the
// fixed preamble its voice is told before the caller's persona, and the marks
// around a span the voice must say exactly. A native adapter (GPT-Live) and an
// emulated one (a speech-to-speech wire with tools) tell the voice the same
// thing, in the same words, so the provider serving a session changes how a
// turn is handed over, never what the voice is asked to do with the answer.

// VerbatimOpen and VerbatimClose mark a span of an answer the voice says
// exactly as written: a number, an address, a name, a code. Kaana writes them
// around each `verbatim` span of a delegation.reply and never relays them back
// in a transcript (StripVerbatimMarks).
const (
	VerbatimOpen  = "⟦"
	VerbatimClose = "⟧"
)

// LivePreamble is what Kaana tells the voice of every `live` session, before
// the caller's persona. It names no mechanism: a native adapter hands a turn
// over by the provider's own delegation, an emulated one by its one tool,
// whose description says how (internal/provider/openairealtime).
const LivePreamble = "You are the voice of a spoken conversation, and you never answer a request yourself. " +
	"Every request the person makes, whatever it is, is answered by delegation: hand it over as soon as the person has made it, " +
	"then speak only the answer that comes back, in the person's language, without adding facts, opinions or offers of your own. " +
	"Text between " + VerbatimOpen + " and " + VerbatimClose + " in an answer is said exactly as written, never reworded, " +
	"translated or spelled differently, and the marks themselves are never said. " +
	"What follows describes who you are and how you speak."

// LiveInstructions is the instructions a `live` session's voice is given: the
// preamble, then the caller's persona when it named one.
func LiveInstructions(persona *string) string {
	if persona == nil || strings.TrimSpace(*persona) == "" {
		return LivePreamble
	}
	return LivePreamble + "\n\n" + *persona
}

// verbatimMarks removes the marks wherever they appear.
var verbatimMarks = strings.NewReplacer(VerbatimOpen, "", VerbatimClose, "")

// StripVerbatimMarks is text without the verbatim marks: what a transcript of
// the voice relays, whether or not the voice read the marks out.
func StripVerbatimMarks(text string) string {
	return verbatimMarks.Replace(text)
}

// WrapVerbatim is a reply's text with every occurrence of each verbatim span
// between the marks. Marks already in the text or a span are removed first,
// so only Kaana's own reach the voice. Where spans overlap, the longest one
// starting first is wrapped and the text it covers is not wrapped again.
func WrapVerbatim(text string, spans []string) string {
	text = StripVerbatimMarks(text)
	cleaned := make([]string, 0, len(spans))
	for _, span := range spans {
		if span = StripVerbatimMarks(span); span != "" {
			cleaned = append(cleaned, span)
		}
	}
	if len(cleaned) == 0 {
		return text
	}
	var wrapped strings.Builder
	for index := 0; index < len(text); {
		longest := ""
		for _, span := range cleaned {
			if len(span) > len(longest) && strings.HasPrefix(text[index:], span) {
				longest = span
			}
		}
		if longest == "" {
			wrapped.WriteByte(text[index])
			index++
			continue
		}
		wrapped.WriteString(VerbatimOpen + longest + VerbatimClose)
		index += len(longest)
	}
	return wrapped.String()
}
