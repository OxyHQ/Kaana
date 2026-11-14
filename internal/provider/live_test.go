package provider

import (
	"strings"
	"testing"
)

// TestVerbatimSpansAreMarkedAndTheMarksNeverRelayed: every occurrence of
// each span is wrapped once, the longest starting first where spans overlap,
// marks the backend wrote itself are removed so only Kaana's reach the voice,
// a reply without spans is its text unchanged, and stripping undoes wrapping.
func TestVerbatimSpansAreMarkedAndTheMarksNeverRelayed(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		spans      []string
		want       string
	}{
		{"no spans", "Your code is 4417.", nil, "Your code is 4417."},
		{"one span", "Your code is 4417.", []string{"4417"}, "Your code is ⟦4417⟧."},
		{"every occurrence", "4417, again 4417", []string{"4417"}, "⟦4417⟧, again ⟦4417⟧"},
		{"longest first", "Call 555 0101 now", []string{"555", "555 0101"}, "Call ⟦555 0101⟧ now"},
		{"marks in the text", "Say ⟦this⟧ and 12", []string{"12"}, "Say this and ⟦12⟧"},
		{"marks in a span", "Room 7B", []string{"⟦7B⟧"}, "Room ⟦7B⟧"},
		{"a span of marks only", "Room 7B", []string{"⟦⟧"}, "Room 7B"},
		{"non-ASCII", "Calle Àlvarez 3, Ñandú", []string{"Àlvarez 3", "Ñandú"}, "Calle ⟦Àlvarez 3⟧, ⟦Ñandú⟧"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WrapVerbatim(tc.text, tc.spans)
			if got != tc.want {
				t.Fatalf("WrapVerbatim = %q, want %q", got, tc.want)
			}
			if stripped := StripVerbatimMarks(got); stripped != StripVerbatimMarks(tc.text) {
				t.Errorf("stripping the marks gives %q, not the text", stripped)
			}
		})
	}
}

// TestTheLivePreambleComesBeforeThePersona: the instructions are the preamble,
// then the persona when there is one; the preamble names both marks, which is
// what tells the voice a marked span is said exactly.
func TestTheLivePreambleComesBeforeThePersona(t *testing.T) {
	persona := "You are Alia. Speak warmly."
	if got := LiveInstructions(&persona); got != LivePreamble+"\n\n"+persona {
		t.Errorf("instructions = %q", got)
	}
	blank := "  "
	for _, none := range []*string{nil, &blank} {
		if got := LiveInstructions(none); got != LivePreamble {
			t.Errorf("instructions without a persona = %q", got)
		}
	}
	if !strings.Contains(LivePreamble, VerbatimOpen) || !strings.Contains(LivePreamble, VerbatimClose) {
		t.Error("the preamble does not name the verbatim marks")
	}
}
