package nativeadmission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyFileRequiresExactReviewedBytes(t *testing.T) {
	raw, err := json.Marshal(PolicyFile{SchemaVersion: 1, Policies: []Policy{policy()}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash := digest(raw)
	if policies, err := LoadPolicyFile(path, hash); err != nil || len(policies) != 1 {
		t.Fatal("reviewed policy refused", err)
	}
	for _, pair := range [][2]string{{path, ""}, {path, digest([]byte("other"))}, {"", hash}} {
		if _, err := LoadPolicyFile(pair[0], pair[1]); err == nil {
			t.Fatal("unreviewed policy accepted")
		}
	}
	if err = os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicyFile(path, hash); err == nil {
		t.Fatal("policy bytes changed under same review")
	}
}
