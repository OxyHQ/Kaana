package keyring_test

import (
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/keyring"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const secretValue = "sk-fake-credential-value-0000"

func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

const good = `{
  "issuedAt": "2026-08-23T00:00:00Z",
  "providers": {
    "openrouter": {
      "keysOnSeparateAccounts": true,
      "keyRetirement": "30m",
      "keys": [
        { "keyId": "or-paid", "secretEnv": "K_PAID", "class": "paid", "budgetUsd": 500 },
        { "keyId": "or-free", "secretEnv": "K_FREE", "class": "free" },
        { "keyId": "or-plain", "secretEnv": "K_PLAIN" }
      ]
    },
    "groq": {
      "keys": [ { "keyId": "gq-1", "secretEnv": "K_GQ" } ]
    }
  }
}`

func goodEnv() func(string) string {
	return env(map[string]string{
		"K_PAID": secretValue + "-1", "K_FREE": secretValue + "-2",
		"K_PLAIN": secretValue + "-3", "K_GQ": secretValue + "-4",
	})
}

func TestAManifestResolvesIntoPools(t *testing.T) {
	pools, err := keyring.Parse([]byte(good), goodEnv())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("pools = %d, want 2", len(pools))
	}
	if pools[0].Provider != "groq" || pools[1].Provider != "openrouter" {
		t.Fatalf("providers = %q, %q; want groq then openrouter", pools[0].Provider, pools[1].Provider)
	}

	or := pools[1]
	if got := or.Policy.Retirement.String(); got != "30m0s" {
		t.Errorf("retirement = %s", got)
	}
	if !or.Policy.OnSeparateAccounts {
		t.Error("keysOnSeparateAccounts was declared true and did not survive")
	}
	if len(or.Declarations) != 3 {
		t.Fatalf("declarations = %d", len(or.Declarations))
	}
	// Parse preserves the written order; the POOL is what reorders. Keeping the
	// two apart means a reader of the manifest sees what they wrote.
	if or.Declarations[0].KeyID != "or-paid" || or.Declarations[1].KeyID != "or-free" {
		t.Errorf("Parse reordered the manifest: %q, %q", or.Declarations[0].KeyID, or.Declarations[1].KeyID)
	}
	if or.Declarations[1].Class != provider.KeyClassFree {
		t.Errorf("class did not survive: %q", or.Declarations[1].Class)
	}
	if or.Declarations[2].Class != provider.KeyClassUnstated {
		t.Errorf("an unstated key acquired class %q", or.Declarations[2].Class)
	}
	if or.Declarations[0].Secret != secretValue+"-1" {
		t.Error("the secret was not resolved from its variable")
	}
	// And the pool built from them puts the free one first.
	pool, err := provider.NewKeyPool("openrouter", or.Declarations, or.Policy, nil)
	if err != nil {
		t.Fatalf("NewKeyPool: %v", err)
	}
	first, ok := pool.Begin().Next(nowForTest())
	if !ok || first.ID != "or-free" {
		t.Errorf("first key = %q, want or-free", first.ID)
	}
}

// A budget nothing enforces is reported, so the caller can say so out loud. An
// operator who declares a cap and is not told it is inert believes they are
// protected, which is worse than having declared nothing.
func TestADeclaredBudgetIsReportedAsUnenforced(t *testing.T) {
	pools, err := keyring.Parse([]byte(good), goodEnv())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := pools[1].UnenforcedBudgets; len(got) != 1 || got[0] != "or-paid" {
		t.Fatalf("unenforced budgets = %v, want [or-paid]", got)
	}
	if len(pools[0].UnenforcedBudgets) != 0 {
		t.Errorf("groq declared no budget and reported %v", pools[0].UnenforcedBudgets)
	}
}

func TestParseRefusesWhatWouldServeNothing(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		getenv   func(string) string
		wants    string
	}{
		{"no providers", `{"providers":{}}`, goodEnv(), "no provider"},
		{"provider with no keys", `{"providers":{"groq":{"keys":[]}}}`, goodEnv(), "no keys"},
		{"key with no id", `{"providers":{"groq":{"keys":[{"secretEnv":"K_GQ"}]}}}`, goodEnv(), "no keyId"},
		{"key naming no credential at all", `{"providers":{"groq":{"keys":[{"keyId":"a"}]}}}`, goodEnv(), "neither secret nor secretEnv"},
		{"key naming a credential twice", `{"providers":{"groq":{"keys":[{"keyId":"a","secretEnv":"K_GQ","secret":"inline-value"}]}}}`, goodEnv(), "two answers"},
		{"variable nobody set", `{"providers":{"groq":{"keys":[{"keyId":"a","secretEnv":"K_MISSING"}]}}}`, goodEnv(), "K_MISSING"},
		{"class nobody defined", `{"providers":{"groq":{"keys":[{"keyId":"a","secretEnv":"K_GQ","class":"cheap"}]}}}`, goodEnv(), "cheap"},
		{"retirement that is not a duration", `{"providers":{"groq":{"keyRetirement":"soon","keys":[{"keyId":"a","secretEnv":"K_GQ"}]}}}`, goodEnv(), "not a duration"},
		{"retirement that returns a key at once", `{"providers":{"groq":{"keyRetirement":"0s","keys":[{"keyId":"a","secretEnv":"K_GQ"}]}}}`, goodEnv(), "not positive"},
		{"not JSON", `{`, goodEnv(), "not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := keyring.Parse([]byte(c.manifest), c.getenv)
			if err == nil {
				t.Fatal("accepted a manifest that would present as configured and serve nothing")
			}
			if !strings.Contains(err.Error(), c.wants) {
				t.Errorf("error does not name what is wrong (%q): %v", c.wants, err)
			}
		})
	}
}

// The positive control for the table above: the same shapes, corrected, parse.
// Without it every case would pass against a Parse that refused everything.
func TestTheRefusalsAreNotJustRefusingEverything(t *testing.T) {
	if _, err := keyring.Parse([]byte(good), goodEnv()); err != nil {
		t.Fatalf("a valid manifest was refused: %v", err)
	}
}

// No error may carry a credential, including the ones about credentials.
func TestNoRefusalCarriesACredential(t *testing.T) {
	manifest := `{"providers":{"groq":{"keys":[{"keyId":"a","secretEnv":"K_GQ","class":"cheap"}]}}}`
	_, err := keyring.Parse([]byte(manifest), goodEnv())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("the error carries the credential: %v", err)
	}
}

func nowForTest() time.Time { return time.Now() }

// Order is asserted over REPEATED parses, not one. Go randomises map iteration
// per range, so a single check against a two-provider manifest passes half the
// time with the sort removed — which is a coin toss wearing a gate's clothes.
func TestProviderOrderIsStableAcrossParses(t *testing.T) {
	first, err := keyring.Parse([]byte(good), goodEnv())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		again, err := keyring.Parse([]byte(good), goodEnv())
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		for i := range first {
			if again[i].Provider != first[i].Provider {
				t.Fatalf("parse %d returned %q at position %d where the first returned %q", attempt, again[i].Provider, i, first[i].Provider)
			}
		}
	}
}

// The inline form, which is what the published snapshot uses. It is the same
// document type read a different way, so a key resolved from it must be
// indistinguishable downstream from one resolved through a variable.
func TestAnInlineSecretResolves(t *testing.T) {
	manifest := `{"providers":{"groq":{"protocol":"openai_compatible","baseURL":"https://api.groq.com/openai/v1","keys":[
	  {"keyId":"gq-free","secret":"` + secretValue + `","class":"free"}
	]}}}`
	pools, err := keyring.Parse([]byte(manifest), env(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(pools) != 1 || len(pools[0].Declarations) != 1 {
		t.Fatalf("pools = %+v", pools)
	}
	declaration := pools[0].Declarations[0]
	if declaration.Secret != secretValue {
		t.Errorf("the inline secret did not resolve")
	}
	if declaration.Class != provider.KeyClassFree {
		t.Errorf("class = %q", declaration.Class)
	}
	// It resolved with an environment that answers nothing, which is the point:
	// the snapshot path must not need a variable per key.
}
