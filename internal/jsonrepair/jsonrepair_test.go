package jsonrepair_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/jsonrepair"
)

func TestRepairTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"fenced with language", "```json\n{\"a\": 1}\n```", `{"a": 1}`},
		{"fenced without language", "```\n[1, 2]\n```", `[1, 2]`},
		{"fenced with surrounding whitespace", "\n\n```JSON\n{\"a\":1}\n```\n", `{"a":1}`},
		{"fenced on one line", "```{\"a\":1}```", `{"a":1}`},
		{"fence never closed", "```json\n{\"a\":1}", `{"a":1}`},
		{"prose before", `Here is the JSON: {"a": 1}`, `{"a": 1}`},
		{"prose after", `{"a": 1} Hope that helps!`, `{"a": 1}`},
		{"prose both sides and a fence after", "Sure!\n```json\n{\"a\": [1, 2]}\n```\nAnything else?", `{"a": [1, 2]}`},
		{"prose with a bracket before the object", `See [1]. {"a": 1}`, `{"a": 1}`},
		{"prose braces inside strings are not structure", `Result: {"a": "}{]["} done`, `{"a": "}{]["}`},
		{"escaped quote in a string", `x {"a": "say \"hi}\""} y`, `{"a": "say \"hi}\""}`},
		{"byte order mark", "\ufeff{\"a\":1}", `{"a":1}`},
		{"trailing comma in object", `{"a": 1, "b": 2,}`, `{"a": 1, "b": 2}`},
		{"trailing comma in array", `[1, 2, 3, ]`, `[1, 2, 3 ]`},
		{"nested trailing commas", "{\"a\": [1, {\"b\": 2,},],\n}", "{\"a\": [1, {\"b\": 2}]\n}"},
		{"a comma in a string stays", `{"a": "x,}", "b": 1,}`, `{"a": "x,}", "b": 1}`},
		{"trailing comma inside prose-wrapped object", `Answer: {"a": 1,} ok`, `{"a": 1}`},
		{"missing closing brace", `{"a": 1`, `{"a": 1}`},
		{"missing closers nested", `{"a": [1, 2, {"b": 3`, `{"a": [1, 2, {"b": 3}]}`},
		{"unterminated string", `{"a": "hello`, `{"a": "hello"}`},
		{"unterminated string after a dangling backslash", `{"a": "hello\`, `{"a": "hello"}`},
		{"unterminated with trailing comma", `{"a": [1, 2,`, `{"a": [1, 2]}`},
		{"unterminated inside a fence", "```json\n{\"a\": [1, 2", `{"a": [1, 2]}`},
		{"unterminated after prose", `Here you go: {"items": ["a", "b"`, `{"items": ["a", "b"]}`},
		{"unicode and escapes survive", `{"a": "café \n \"q\" é 日本語",}`, `{"a": "café \n \"q\" é 日本語"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := jsonrepair.Repair(c.in)
			if !changed || got != c.want {
				t.Fatalf("Repair(%q) = %q, %v; want %q, true", c.in, got, changed, c.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("Repair(%q) = %q is not valid JSON", c.in, got)
			}
		})
	}
}

func TestValidTextIsReturnedByteIdentical(t *testing.T) {
	for _, in := range []string{
		`{"a":1}`, "  {\"a\": [1, 2]}\n", `[]`, `"text"`, `42`, `null`, `true`,
		"{\"a\": \"```json\"}", "{\n  \"a\": \"x,}\"\n}\n\n",
	} {
		got, changed := jsonrepair.Repair(in)
		if changed || got != in {
			t.Errorf("Repair(%q) = %q, %v; valid text must come back unchanged", in, got, changed)
		}
	}
}

func TestUnrepairableTextIsReturnedUntouched(t *testing.T) {
	for _, in := range []string{
		"", "   ", "I cannot help with that.", "```json\n```", `{"a": }`, `{"a":`, `{"a": 1, "b"`, `{"ab`,
		`{"a": tru`, `{'a': 1}`, `{a: 1}`, `[1, 2` + "\x00", `}{`, `{"a": 1}}`, `{"a": 1} {"b": 2}`,
		`{"a" 1}`, `[1.`, `{"a": "\u00`,
	} {
		got, changed := jsonrepair.Repair(in)
		if changed || got != in {
			t.Errorf("Repair(%q) = %q, %v; unrepairable text must come back untouched", in, got, changed)
		}
	}
}

func TestNothingIsInvented(t *testing.T) {
	// A key with no value and a half-written literal are not completed.
	for _, in := range []string{`{"a": 1, "b":`, `{"a": [1, nul`, `{"a": 1, "b"`} {
		if got, changed := jsonrepair.Repair(in); changed {
			t.Errorf("Repair(%q) = %q; a missing value must not be made up", in, got)
		}
	}
}

func TestHugeTextIsNotAttempted(t *testing.T) {
	big := "```json\n" + `{"a": "` + strings.Repeat("x", jsonrepair.MaxBytes) + `"}` + "\n```"
	if got, changed := jsonrepair.Repair(big); changed || got != big {
		t.Fatalf("a text over %d bytes was repaired", jsonrepair.MaxBytes)
	}
	small := "```json\n{\"a\": \"" + strings.Repeat("x", 1<<20) + "\"}\n```"
	if _, changed := jsonrepair.Repair(small); !changed {
		t.Fatal("a 1 MiB fenced answer was not repaired")
	}
}

func TestManyBracketsInProseStayBounded(t *testing.T) {
	prose := strings.Repeat("[ { ", 100000) + `{"a": 1`
	if len(prose) > jsonrepair.MaxBytes {
		t.Fatal("test input over the cap")
	}
	jsonrepair.Repair(prose)
}

func TestRepairIsDeterministicAndIdempotent(t *testing.T) {
	in := "Sure:\n```json\n{\"a\": [1, 2,],}\n```"
	first, changed := jsonrepair.Repair(in)
	if !changed {
		t.Fatal("not repaired")
	}
	second, again := jsonrepair.Repair(first)
	if again || second != first {
		t.Fatalf("repair is not idempotent: %q then %q", first, second)
	}
}

func FuzzRepair(f *testing.F) {
	for _, seed := range []string{
		"", `{"a":1}`, "```json\n{\"a\":1}\n```", `x {"a": [1,2,]} y`, `{"a": "b`, `[1, [2, {"c": "d\`,
		"\ufeff[]", `{"a": "}{]["}`, "```", "```json", `{`, `[`, `"`, `{"a":1,`, "[1, 2,\n]", `Sure! {"a": {"b": [1, 2`,
		`{"a": "é\"`, `}{`, `{"a":{"b":{"c":`, "```\n```",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, changed := jsonrepair.Repair(in)
		if json.Valid([]byte(in)) && (changed || out != in) {
			t.Fatalf("valid input %q was changed to %q", in, out)
		}
		if !changed && out != in {
			t.Fatalf("unchanged result %q differs from input %q", out, in)
		}
		if changed && !json.Valid([]byte(out)) {
			t.Fatalf("repaired %q to invalid %q", in, out)
		}
	})
}
