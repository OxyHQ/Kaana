// Package jsonrepair makes nearly valid JSON valid, deterministically and
// without inventing anything. It is the whole of Kaana's opt-in response
// repair (docs/json-repair.md): a pure function of one string, with no schema,
// no model and no I/O.
//
// The repairs run in a fixed order and stop at the first that yields one JSON
// value:
//
//  1. trim whitespace and a byte order mark;
//  2. strip one surrounding markdown code fence;
//  3. extract the first balanced top-level object or array from surrounding prose;
//  4. remove trailing commas before a closing brace or bracket;
//  5. close an unterminated string and unclosed brackets at the end of the text.
//
// Text that already parses is returned byte-identical, and text no step can
// make valid is returned untouched. Nothing is reordered, quoted, unquoted or
// supplied: a missing value or key is never guessed.
package jsonrepair

import (
	"encoding/json"
	"strings"
)

// MaxBytes is the largest text Repair will look at. A larger text is returned
// untouched: repair is a convenience for an answer a model typed, not a parser
// for a bulk document, and the bound keeps its cost linear and small.
const MaxBytes = 4 << 20

// maxStarts bounds how many candidate opening brackets in surrounding prose
// are tried, so prose full of brackets cannot make repair quadratic.
const maxStarts = 16

// maxDepth is the nesting closeAtEnd will close; encoding/json refuses deeper
// text anyway.
const maxDepth = 10000

const byteOrderMark = "\ufeff"

// Repair returns the repaired text and whether it differs from the input. When
// it returns false the text is the input, byte for byte.
func Repair(text string) (string, bool) {
	if len(text) > MaxBytes || valid(text) {
		return text, false
	}
	if repaired, ok := attempt(text); ok && repaired != text {
		return repaired, true
	}
	return text, false
}

func valid(text string) bool { return json.Valid([]byte(text)) }

func attempt(text string) (string, bool) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), byteOrderMark))
	if valid(trimmed) {
		return trimmed, true
	}
	unfenced := stripFence(trimmed)
	if valid(unfenced) {
		return unfenced, true
	}
	starts := openings(unfenced)
	// A balanced fragment that parses as it is.
	var fragments []string
	for _, start := range starts {
		// What follows the fragment must be prose, and so must what precedes it
		// (a footnote "[1]" before an object is prose): a second structure, or a
		// stray closer, would be dropped by extracting one.
		forbidden := "{}"
		if unfenced[start] == '[' {
			forbidden = "{}[]"
		}
		if end := balancedEnd(unfenced, start); end > start && !strings.ContainsAny(unfenced[end:], "{}[]") && !strings.ContainsAny(unfenced[:start], forbidden) {
			fragment := unfenced[start:end]
			if valid(fragment) {
				return fragment, true
			}
			fragments = append(fragments, fragment)
		}
	}
	// The whole text, then each balanced fragment, without trailing commas.
	candidates := append([]string{unfenced}, fragments...)
	for _, candidate := range candidates {
		if cleaned := dropTrailingCommas(candidate); cleaned != candidate && valid(cleaned) {
			return cleaned, true
		}
	}
	// A normal stop that left the structure open: from an opening bracket to
	// the end of the text.
	for _, start := range starts {
		if closed, ok := closeAtEnd(unfenced[start:]); ok {
			return closed, true
		}
	}
	return "", false
}

// stripFence removes one surrounding markdown code fence: an opening line of
// three backticks and an optional language word, and a closing fence if there
// is one. A text that does not begin with a fence is returned as it is.
func stripFence(text string) string {
	rest, found := strings.CutPrefix(text, "```")
	if !found {
		return text
	}
	body := rest
	if line, after, hasNewline := strings.Cut(rest, "\n"); hasNewline && isLanguageWord(strings.TrimSpace(line)) {
		body = after
	}
	body = strings.TrimSpace(body)
	return strings.TrimSpace(strings.TrimSuffix(body, "```"))
}

func isLanguageWord(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '+' {
			return false
		}
	}
	return true
}

// openings is the offsets of up to maxStarts opening brackets.
func openings(text string) []int {
	var starts []int
	for i := 0; i < len(text) && len(starts) < maxStarts; i++ {
		if text[i] == '{' || text[i] == '[' {
			starts = append(starts, i)
		}
	}
	return starts
}

// balancedEnd is the offset just past the bracket that closes the one opening
// at start, or -1 when the text ends first or the brackets do not match.
func balancedEnd(text string, start int) int {
	var stack []byte
	inString, escaped := false, false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return -1
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// dropTrailingCommas removes each comma that is followed, past whitespace, by
// a closing brace or bracket. Commas inside strings are never touched.
func dropTrailingCommas(text string) string {
	var out strings.Builder
	inString, escaped, changed := false, false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			out.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
		}
		if c == ',' {
			j := i + 1
			for j < len(text) && isSpace(text[j]) {
				j++
			}
			if j < len(text) && (text[j] == '}' || text[j] == ']') {
				changed = true
				continue
			}
		}
		out.WriteByte(c)
	}
	if !changed {
		return text
	}
	return out.String()
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// closeAtEnd closes an unterminated string and the open brackets of text,
// which is cut off at its end. It adds only closing quotes and brackets, and
// reports false when that does not make the text valid (a key with no value,
// a half-written literal): a value is never made up.
func closeAtEnd(text string) (string, bool) {
	var stack []byte
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return "", false
			}
			stack = stack[:len(stack)-1]
		}
		if len(stack) > maxDepth {
			return "", false
		}
	}
	// An opening bracket with nothing after it is not a structure to finish.
	if (len(stack) == 0 && !inString) || strings.TrimSpace(text[1:]) == "" {
		return "", false
	}
	head := text
	if inString {
		if escaped {
			head = head[:len(head)-1]
		}
		head += `"`
	}
	head = strings.TrimSuffix(strings.TrimRight(head, " \t\r\n"), ",")
	var closed strings.Builder
	closed.Grow(len(head) + len(stack))
	closed.WriteString(head)
	for i := len(stack) - 1; i >= 0; i-- {
		closed.WriteByte(stack[i])
	}
	head = dropTrailingCommas(closed.String())
	if !valid(head) {
		return "", false
	}
	return head, true
}
