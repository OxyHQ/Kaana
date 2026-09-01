package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCredentialInputIsOneBoundedStdinLine(t *testing.T) {
	secret, err := readCredential(strings.NewReader("provider-secret\r\n"))
	if err != nil {
		t.Fatalf("readCredential: %v", err)
	}
	if string(secret) != "provider-secret" {
		t.Fatalf("secret = %q", secret)
	}
	clear(secret)

	for name, input := range map[string][]byte{
		"empty":     nil,
		"two lines": []byte("one\ntwo\n"),
		"oversized": bytes.Repeat([]byte("x"), maxCredentialBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readCredential(bytes.NewReader(input)); err == nil {
				t.Fatal("unsafe credential input was accepted")
			}
		})
	}
}

func TestBudgetMetadataCannotBeNegative(t *testing.T) {
	if _, err := parseBudget("-0.01"); err == nil {
		t.Fatal("negative budget was accepted")
	}
	value, err := parseBudget("12.50")
	if err != nil || value == nil || *value != 12.5 {
		t.Fatalf("valid budget = %v, %v", value, err)
	}
}
