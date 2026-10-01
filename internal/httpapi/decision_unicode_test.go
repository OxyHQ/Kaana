package httpapi

import "testing"

func TestDecisionJSONUnicode(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"high", "{\"state\":\"\\ud800\"}", false},
		{"low", "{\"state\":\"\\udfff\"}", false},
		{"high-high", "{\"state\":\"\\ud800\\ud800\"}", false},
		{"high-scalar", "{\"state\":\"\\ud800\\u0041\"}", false},
		{"separated", "[\"\\ud800\",\"\\udc00\"]", false},
		{"root-key", "{\"\\ud800\":1}", false},
		{"nested-option", "{\"questions\":[{\"options\":[\"safe\",\"\\ud800\"]}]}", false},
		{"pair", "{\"state\":\"\\ud83d\\ude00\"}", true},
		{"uppercase-pair", "\"\\uD83D\\uDE00\"", true},
		{"pair-boundaries", "[\"\\ud800\\udc00\",\"\\udbff\\udfff\"]", true},
		{"replacement-literal", "{\"state\":\"�\"}", true},
		{"replacement-escape", "\"\\ufffd\"", true},
		{"unicode-literal", "{\"state\":\"😀 español 中文\"}", true},
		{"escaped-backslash", "{\"state\":\"\\\\ud800\"}", true},
		{"escaped-quote", "{\"state\":\"\\\" safe \\ud83d\\ude00\"}", true},
		{"malformed-hex", "\"\\uZZZZ\"", false},
		{"truncated-escape", "\"\\uD800", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDecisionJSONUnicode([]byte(tc.body))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
