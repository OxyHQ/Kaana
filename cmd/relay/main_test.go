package main

import (
	"strings"
	"testing"
)

// TestFailoverAcknowledgementCannotBeSetByAccident.
//
// Same-model failover overrides a routing-policy control the envelope does not
// carry, so enabling it is a statement someone makes on purpose. The parser is
// what makes that true: an empty variable, a bare "true", a "yes", or a reason
// with no date all leave the safe default in place or refuse to start, and none
// of them turns it on.
func TestFailoverAcknowledgementCannotBeSetByAccident(t *testing.T) {
	refusedOrOff := []struct {
		name  string
		value string
		fails bool
	}{
		{name: "unset", value: "", fails: false},
		{name: "whitespace", value: "   ", fails: false},
		{name: "a bare truthy word", value: "true", fails: true},
		{name: "an enthusiastic yes", value: "yes", fails: true},
		{name: "a reason with no date", value: "alia-canary:", fails: true},
		{name: "a date with no reason", value: ":2026-08-16", fails: true},
		{name: "a date nobody can parse", value: "alia-canary:soon", fails: true},
		{name: "a date in the wrong order", value: "alia-canary:16-08-2026", fails: true},
	}

	for _, testCase := range refusedOrOff {
		t.Run(testCase.name, func(t *testing.T) {
			acknowledgement, err := failoverAcknowledgement(testCase.value)
			if testCase.fails && err == nil {
				t.Fatalf("%q was accepted", testCase.value)
			}
			if !testCase.fails && err != nil {
				t.Fatalf("%q was refused: %v", testCase.value, err)
			}
			if acknowledgement != "" {
				t.Errorf("%q enabled failover", testCase.value)
			}
		})
	}

	// The control: a well-formed acknowledgement does enable it, or every case
	// above would pass on a parser that refuses everything.
	acknowledgement, err := failoverAcknowledgement(" alia-first-party-canary:2026-08-16 ")
	if err != nil {
		t.Fatalf("a well-formed acknowledgement was refused: %v", err)
	}
	if !strings.Contains(acknowledgement, "alia-first-party-canary") {
		t.Errorf("the acknowledgement is recorded as %q; it names who accepted it", acknowledgement)
	}
}
