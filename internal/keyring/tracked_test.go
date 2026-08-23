package keyring_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest carrying inline secrets is a credential store. The publisher
// renders one and delivers it through the sidecar; nothing produces one into
// this repository, and nobody should.
//
// This is the accident the `secret` field makes possible: an operator debugging
// a deployment pastes a working manifest into `configs/` to reproduce
// something, and commits it. No review catches a diff that looks exactly like
// the example file, so the check has to be mechanical.
func inlineSecretsIn(t *testing.T, raw []byte) []string {
	t.Helper()
	var document struct {
		Providers map[string]struct {
			Keys []struct {
				KeyID  string `json:"keyId"`
				Secret string `json:"secret"`
			} `json:"keys"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		// Not a manifest, or not JSON. Either way it declares no key.
		return nil
	}
	found := make([]string, 0)
	for slug, config := range document.Providers {
		for _, key := range config.Keys {
			if strings.TrimSpace(key.Secret) != "" {
				found = append(found, slug+"/"+key.KeyID)
			}
		}
	}
	return found
}

func TestNoTrackedFileCarriesAnInlineSecret(t *testing.T) {
	out, err := exec.Command("git", "-C", "../..", "ls-files", "-z", "--", "*.json").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 })

	// Vacuity floor. An empty listing — a moved repository, a `git` that
	// answered nothing — is what "no file carries a secret" also looks like.
	if len(files) < 3 {
		t.Fatalf("git listed %d tracked JSON files; this check would pass over nothing", len(files))
	}

	var scanned int
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		scanned++
		if found := inlineSecretsIn(t, raw); len(found) > 0 {
			t.Errorf("%s carries inline credentials for %v; a manifest in this repository names variables, never values", name, found)
		}
	}
	t.Logf("scanned %d tracked JSON files", scanned)
}

// The positive control. Without it the scan above passes whether or not the
// detector can see anything at all — which is precisely how a census reports
// clean while measuring nothing.
func TestTheInlineSecretDetectorCanSeeOne(t *testing.T) {
	document := []byte(`{"providers":{"groq":{"keys":[
	  {"keyId":"safe","secretEnv":"K"},
	  {"keyId":"leaked","secret":"sk-a-real-looking-value"}
	]}}}`)
	found := inlineSecretsIn(t, document)
	if len(found) != 1 || found[0] != "groq/leaked" {
		t.Fatalf("the detector found %v, want [groq/leaked]", found)
	}
}
