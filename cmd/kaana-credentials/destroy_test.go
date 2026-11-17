package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDestroyParsesOnlyExactSelectors(t *testing.T) {
	operation, err := parseDestroyOperation([]string{
		"--operation-id", "kcd_0123456789abcdef0123456789abcdef",
		"--provider", "elevenlabs", "--key-id", "9b2f6c1e-4d3a-4f7b-8e21-5c0d7a6b3f10",
	}, "github-actions:OxyHQ/Kaana:1")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Provider != "elevenlabs" || operation.KeyID != "9b2f6c1e-4d3a-4f7b-8e21-5c0d7a6b3f10" || operation.Actor != "github-actions:OxyHQ/Kaana:1" {
		t.Fatalf("operation = %+v", operation)
	}
	for name, arguments := range map[string][]string{
		"no actor":     {"--operation-id", "kcd_0123456789abcdef0123456789abcdef", "--provider", "groq", "--key-id", "k"},
		"kop id":       {"--operation-id", "kop_0123456789abcdef0123456789abcdef", "--provider", "groq", "--key-id", "k"},
		"missing key":  {"--operation-id", "kcd_0123456789abcdef0123456789abcdef", "--provider", "groq"},
		"force flag":   {"--operation-id", "kcd_0123456789abcdef0123456789abcdef", "--provider", "groq", "--key-id", "k", "--force"},
		"positional":   {"--operation-id", "kcd_0123456789abcdef0123456789abcdef", "--provider", "groq", "--key-id", "k", "extra"},
		"bad provider": {"--operation-id", "kcd_0123456789abcdef0123456789abcdef", "--provider", "Groq Cloud", "--key-id", "k"},
	} {
		actor := "operator:test"
		if name == "no actor" {
			actor = ""
		}
		if _, err := parseDestroyOperation(arguments, actor); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDestroyRefusesBeforeOpeningAnything(t *testing.T) {
	var stdout bytes.Buffer
	err := run([]string{"destroy", "--operation-id", "bad", "--provider", "groq", "--key-id", "k"}, strings.NewReader(""), &stdout, func(name string) string {
		if name == "KAANA_CREDENTIAL_ACTOR" {
			return "operator:test"
		}
		return ""
	})
	if err == nil || stdout.Len() != 0 {
		t.Fatalf("run = %v, stdout %q", err, stdout.String())
	}
}
