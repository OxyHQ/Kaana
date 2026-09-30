package openaicompat

import (
	"encoding/json"
	"testing"
)

func TestTheErrorEnvelopeReadsBothShapes(t *testing.T) {
	for name, testCase := range map[string]struct {
		raw         string
		wantMessage string
		wantType    string
	}{
		// xAI: `error` is the message itself (measured against production xAI).
		"xAI flat": {`{"code":"Client specified an invalid argument","error":"Model grok-build-0.1 does not support parameter reasoningEffort."}`,
			"Model grok-build-0.1 does not support parameter reasoningEffort.", ""},
		// OpenAI and every provider copying it: an object.
		"OpenAI object": {`{"error":{"message":"slow down","type":"rate_limit_exceeded"}}`, "slow down", "rate_limit_exceeded"},
		"no error":      {`{"code":"x"}`, "", ""},
	} {
		var body upstreamErrorBody
		if err := json.Unmarshal([]byte(testCase.raw), &body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if body.Error.Message != testCase.wantMessage || body.Error.Type != testCase.wantType {
			t.Errorf("%s: read %+v", name, body.Error)
		}
	}
}
