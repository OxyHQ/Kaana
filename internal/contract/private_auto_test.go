package contract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func privateAutoGolden(t *testing.T, file string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/private-auto/golden-" + file + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestPrivateAutoCrossRuntimeCanonicalAuthorityAndInputs(t *testing.T) {
	var approval PrivateAutoSourceApproval
	if err := json.Unmarshal(privateAutoGolden(t, "approval"), &approval); err != nil {
		t.Fatal(err)
	}
	hash, err := approval.SHA256()
	if err != nil || hash != "126882e06843972f2486ca1187eb6cd03fb37716ffde952be5eb888583237ebb" {
		t.Fatal("source encoding differs from Oxy", hash, err)
	}
	for _, name := range []string{"first", "unicode"} {
		t.Run(name, func(t *testing.T) {
			var wire PrivateAutoRequest
			if err := json.Unmarshal(privateAutoGolden(t, name), &wire); err != nil {
				t.Fatal(err)
			}
			request := wire.InferenceRequest()
			if !request.PrivateAutoExecution.MatchesSource(&approval, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
				t.Fatal("source match differs")
			}
			if err := request.ValidatePrivateAutoInputBytes(privateAutoGolden(t, name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestPrivateAutoRejectsMissingDuplicateAndUnknownAuthorityMembers(t *testing.T) {
	raw := string(privateAutoGolden(t, "approval"))
	for _, bad := range []string{
		strings.Replace(raw, `"commercialUseAllowed":false,`, "", 1),
		strings.Replace(raw, `"commercialUseAllowed":false`, `"commercialUseAllowed":false,"commercialUseAllowed":true`, 1),
		strings.Replace(raw, `"lane":"service_token"`, `"lane":"service_token","lane":"service_token"`, 1),
		strings.Replace(raw, `"retainsPayloads":false`, `"retainsPayloads":null`, 1),
		strings.Replace(raw, `"limits":{`, `"limits":{"extra":1,`, 1),
		strings.Replace(raw, `"maxCostUsd":"0.001000000000"`, `"maxCostUsd":"0.001000000001"`, 1),
	} {
		var approval PrivateAutoSourceApproval
		if err := json.Unmarshal([]byte(bad), &approval); err == nil {
			t.Fatal("ambiguous/invalid source admitted")
		}
	}
}
func TestPrivateAutoStableParentIdentityAndEnvelopeRefusals(t *testing.T) {
	raw := string(privateAutoGolden(t, "first"))
	var original PrivateAutoRequest
	if err := json.Unmarshal([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"modifiedstate":   strings.Replace(raw, "synthetic variable task", "other task", 1),
		"template":        strings.Replace(raw, PrivateAutoInstructions, "caller authored instructions", 1),
		"two seconds":     strings.Replace(raw, "00:00:01.000Z", "00:00:02.000Z", 1),
		"delegate":        strings.Replace(raw, `"attribution":{`, `"attribution":{"userId":"owner-as-user",`, 1),
		"scope":           strings.Replace(raw, "inference:invoke", "inference:models:read", 1),
		"mixedversion":    strings.Replace(raw, `"schemaVersion":4`, `"schemaVersion":3`, 1),
		"mixedauthority":  strings.Replace(raw, `"schemaVersion":4`, `"scopedExecution":{},"schemaVersion":4`, 1),
		"duplicate":       strings.Replace(raw, `"schemaVersion":4`, `"schemaVersion":4,"schemaVersion":4`, 1),
		"foreignprovider": strings.Replace(raw, `"provider":"openrouter"`, `"provider":"groq"`, 1),
		"levels":          strings.Replace(raw, `"kind":"choice"`, `"levels":[],"kind":"choice"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var wire PrivateAutoRequest
			if err := json.Unmarshal([]byte(bad), &wire); err == nil {
				t.Fatal("invalid private child admitted")
			}
		})
	}
	var approval PrivateAutoSourceApproval
	_ = json.Unmarshal(privateAutoGolden(t, "approval"), &approval)
	changed := approval
	changed.ApprovalVersion++
	child := original.PrivateAutoExecution
	if child.MatchesSource(&changed, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("approval revision silently renewed")
	}
	changedID, err := PrivateAutoOperationID(child.ParentMeteredUsageID)
	if err != nil || changedID != child.OperationID {
		t.Fatal("child ID contains mutable authority")
	}
	child.PrivateAutoAuthority = changed.PrivateAutoAuthority
	child.ApprovalSHA256, _ = changed.SHA256()
	if child.OperationID != changedID {
		t.Fatal("approval changed stable parent operation")
	}
}
