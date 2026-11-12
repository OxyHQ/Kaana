package reasoningblock

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSealAndOpenRoundTrip(t *testing.T) {
	token, err := Seal(FamilyAnthropic, map[string]string{"thinking": "plan", "signature": "sig"})
	if err != nil {
		t.Fatal(err)
	}
	family, payload, err := Open(token)
	if err != nil || family != FamilyAnthropic {
		t.Fatalf("Open = %q, %v", family, err)
	}
	var back map[string]string
	if err := json.Unmarshal(payload, &back); err != nil || back["signature"] != "sig" || back["thinking"] != "plan" {
		t.Fatalf("payload = %s", payload)
	}
}

func TestPayloadsKeepOwnFamilyInOrderAndDropTheRest(t *testing.T) {
	a1, _ := Seal(FamilyAnthropic, 1)
	o1, _ := Seal(FamilyOpenRouter, 2)
	a2, _ := Seal(FamilyAnthropic, 3)
	got := Payloads(FamilyAnthropic, []string{a1, o1, "not-a-token", a2, ""})
	if len(got) != 2 || string(got[0]) != "1" || string(got[1]) != "3" {
		t.Fatalf("Payloads = %s", got)
	}
}

func TestUnreadableTokenDoesNotEchoItself(t *testing.T) {
	_, _, err := Open("secret-reasoning-text!")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v", err)
	}
}
