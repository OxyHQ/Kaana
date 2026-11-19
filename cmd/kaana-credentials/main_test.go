package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
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

func TestRekeyCLIParsesOnlyExactNonSecretSelectors(t *testing.T) {
	operation, err := parseRekeyOperation([]string{
		"--operation-id", "kop_5b4f96c394a7a288754a1388fed0c5b2",
		"--provider", "cerebras",
		"--old-key-id", "cerebras-relay-main",
		"--new-key-id", "43405cea-a7d1-49c2-ba73-5a84536d3abf",
	}, "github-actions:OxyHQ/Kaana:123")
	if err != nil {
		t.Fatalf("parseRekeyOperation: %v", err)
	}
	if operation.Provider != "cerebras" || operation.SourceKeyID != "cerebras-relay-main" ||
		operation.DestinationKeyID != "43405cea-a7d1-49c2-ba73-5a84536d3abf" ||
		operation.Actor != "github-actions:OxyHQ/Kaana:123" {
		t.Fatalf("operation = %+v", operation)
	}
	if _, err := parseRekeyOperation([]string{
		"--operation-id", operation.OperationID,
		"--provider", "cerebras",
		"--old-key-id", "cerebras-relay-main",
		"--new-key-id", operation.DestinationKeyID,
		"--value", "must-never-enter-argv",
	}, operation.Actor); err == nil {
		t.Fatal("a provider-secret value flag was accepted")
	}
	conditional, err := parseRekeyOperation([]string{
		"--operation-id", "kop_c1b4d87bf4e2a5a6d815dc1a1b0460a3",
		"--provider", "groq",
		"--old-key-id", "relay-groq-20260902",
		"--new-key-id", "f0c4e09f-a5f8-4af8-86b4-960e2d637ce1",
		"--requires-operation-id", "kop_0af8007d9fdddd88d2622eabff99aeb9",
		"--requires-outcome", "different",
	}, "operator:test")
	if err != nil || conditional.PrerequisiteOperationID != "kop_0af8007d9fdddd88d2622eabff99aeb9" ||
		conditional.PrerequisiteOutcome != credentialstore.CredentialAdminOutcomeDifferent {
		t.Fatalf("conditional rekey = %+v, %v", conditional, err)
	}
	if _, err := parseRekeyOperation([]string{
		"--operation-id", "kop_c1b4d87bf4e2a5a6d815dc1a1b0460a3",
		"--provider", "groq",
		"--old-key-id", "relay-groq-20260902",
		"--new-key-id", "f0c4e09f-a5f8-4af8-86b4-960e2d637ce1",
		"--requires-outcome", "different",
	}, "operator:test"); err == nil {
		t.Fatal("a prerequisite outcome without an exact operation id was accepted")
	}
}

func TestDeduplicateCLIMapsKeepAndDuplicateWithoutPositionSelection(t *testing.T) {
	operation, err := parseDeduplicationOperation([]string{
		"--operation-id", "kop_0af8007d9fdddd88d2622eabff99aeb9",
		"--provider", "groq",
		"--duplicate-key-id", "relay-groq-20260902",
		"--keep-key-id", "legacy-alia-20260901",
	}, "operator:test")
	if err != nil {
		t.Fatalf("parseDeduplicationOperation: %v", err)
	}
	expected := credentialstore.CredentialIDOperation{
		OperationID: "kop_0af8007d9fdddd88d2622eabff99aeb9",
		Provider:    "groq", SourceKeyID: "relay-groq-20260902",
		DestinationKeyID: "legacy-alia-20260901", Actor: "operator:test",
	}
	if operation != expected {
		t.Fatalf("operation = %+v, want %+v", operation, expected)
	}
	if _, err := parseDeduplicationOperation([]string{
		"--operation-id", operation.OperationID,
		"--provider", "groq",
		"--duplicate-key-id", operation.SourceKeyID,
		"--keep-key-id", operation.DestinationKeyID,
		"--position", "2",
	}, operation.Actor); err == nil {
		t.Fatal("deduplication accepted position as authority")
	}
}

func TestCredentialAdminReceiptOutputContainsNoSecretMaterial(t *testing.T) {
	receipt := credentialstore.CredentialAdminReceipt{
		OperationID: "kop_0af8007d9fdddd88d2622eabff99aeb9",
		Action:      credentialstore.CredentialAdminActionDeduplicate,
		Provider:    "groq", SourceKeyID: "relay-groq-20260902",
		DestinationKeyID: "legacy-alia-20260901",
		Outcome:          credentialstore.CredentialAdminOutcomeDeduplicated,
	}
	var output bytes.Buffer
	if err := writeReceipt(&output, receipt); err != nil {
		t.Fatalf("writeReceipt: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatalf("receipt JSON: %v", err)
	}
	expectedFields := []string{"operationId", "action", "provider", "sourceKeyId", "destinationKeyId", "outcome", "replayed"}
	if len(fields) != len(expectedFields) {
		t.Fatalf("receipt fields = %v", fields)
	}
	for _, field := range expectedFields {
		if _, present := fields[field]; !present {
			t.Errorf("receipt is missing %q", field)
		}
	}
	for _, forbidden := range []string{"secret", "ciphertext", "digest", "fingerprint"} {
		if strings.Contains(strings.ToLower(output.String()), forbidden) {
			t.Errorf("receipt output contains forbidden material label %q: %s", forbidden, output.String())
		}
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
