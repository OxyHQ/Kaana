// Package keyring reads the declaration of which credentials this deployment
// holds, and turns it into the pools the provider adapters spend from.
//
// # Why a file and not more environment variables
//
// The environment expressed a pool as `KAANA_PROVIDER_<SLUG>_API_KEY`, a
// comma-separated list, plus five more variables per provider for its protocol,
// address, headers, retirement and whether its keys sit on separate accounts.
// That shape holds two or three keys. It does not hold twenty or thirty: one
// string is rotated as a whole, one bad character breaks every key in it, and
// there is nowhere to say that this key is a free tier and that one holds a
// balance somebody paid for.
//
// Six variables per provider collapse into one declaration here.
//
// # The secret is not in the file
//
// Exactly as `configs/inventory.json` says of itself, a credential never
// appears in this document. A key declares the NAME of the variable holding it,
// and the value arrives the way every other secret in this deployment arrives —
// injected by the platform, from the store that owns it. That keeps the serving
// process free of any dependency on a secrets API: a manifest that held SSM
// paths would make a process that cannot start when AWS is unreachable, which
// is a poor trade for a data plane whose job is to be available.
//
// # What it refuses
//
// Every refusal here is a state that would otherwise present as a working
// deployment and behave as a broken one: a key naming a variable nobody set, a
// class nobody defined, two keys under one name, a provider declaring no keys
// at all. Each is named with what is missing, because "some provider has a
// problem" is not something an operator can act on at three in the morning.
package keyring

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// Manifest is the whole declaration.
type Manifest struct {
	// IssuedAt is when this declaration was written. It is recorded for the
	// same reason the inventory records it — so a stale file is a fact rather
	// than a guess — but nothing here expires: a credential does not become
	// wrong because the document naming it is old.
	IssuedAt  string                       `json:"issuedAt"`
	Providers map[string]ProviderKeyConfig `json:"providers"`
}

// ProviderKeyConfig is everything one provider needs: where it is, what it
// speaks, and which credentials this deployment holds for it.
type ProviderKeyConfig struct {
	// Protocol is "openai_compatible" or "anthropic_messages". Empty takes the
	// build's default for a known slug, and is required for any other.
	Protocol string `json:"protocol"`
	// BaseURL is the provider's API root. Empty takes the build's default for a
	// known slug, and is required for any other — an address this build guessed
	// would be an address nobody chose.
	BaseURL string `json:"baseURL"`
	// Headers are the non-secret headers this provider expects on every
	// request. OpenRouter's attribution headers are why the field exists: a
	// provider wired without them is compliant with its terms until it is not,
	// and nothing fails in between.
	Headers map[string]string `json:"headers"`
	// KeysOnSeparateAccounts states that these keys belong to DIFFERENT
	// provider accounts, which is a fact only whoever provisioned them knows.
	// It governs exactly one behaviour — whether a throttle may rotate — and
	// defaults to false, which never rotates.
	KeysOnSeparateAccounts bool `json:"keysOnSeparateAccounts"`
	// KeyRetirement is how long a spent or refused key stays out, as a Go
	// duration. Empty takes provider.DefaultKeyRetirement.
	KeyRetirement string `json:"keyRetirement"`
	Keys          []Key  `json:"keys"`
}

// Key is one credential, by reference.
type Key struct {
	// KeyID names the key to operators, in logs and health projections. It is
	// never derived from the secret.
	KeyID string `json:"keyId"`
	// SecretEnv is the NAME of the environment variable holding the credential.
	// It is the form a manifest kept in this repository uses, because a name is
	// not a secret and a repository is not a place for one.
	SecretEnv string `json:"secretEnv"`
	// Secret is the credential itself, and it exists for ONE producer: the
	// snapshot the publisher renders from the key database and delivers through
	// the same sidecar the inventory arrives by. That path is what makes adding
	// a key a row rather than a deployment — a manifest of variable names
	// cannot, because every new name needs a task-definition entry to inject it.
	//
	// A DOCUMENT CARRYING THIS IS A CREDENTIAL STORE and must be treated as one:
	// encrypted at rest, readable by the task role and nothing else, and never
	// committed. `TestNoTrackedFileCarriesAnInlineSecret` enforces the last of
	// those, because it is the one a person does by accident.
	//
	// Exactly one of Secret and SecretEnv, per key. Both would be two answers to
	// which credential this is, and silently preferring one is how a rotated key
	// keeps serving the old value.
	Secret string `json:"secret,omitempty"`
	// Class is what spending on this key costs: "free", "paid", or absent.
	// Absent is not a synonym for paid — see provider.KeyClass.
	Class string `json:"class"`
	// BudgetUSD caps what may be spent on this key. Declared here so the
	// document is complete; NOTHING IN THIS BUILD ENFORCES IT, and Load says so
	// out loud rather than letting an operator believe a cap is holding.
	BudgetUSD *float64 `json:"budgetUsd,omitempty"`
}

// Pool is one provider's resolved declaration.
type Pool struct {
	Provider     contract.ProviderSlug
	Protocol     string
	BaseURL      string
	Headers      map[string]string
	Declarations []provider.KeyDeclaration
	Policy       provider.KeyPolicy
	// UnenforcedBudgets names the keys that declared a budget this build cannot
	// hold them to. The caller warns; it is not an error, because refusing to
	// boot over a field that is merely aspirational would be worse.
	UnenforcedBudgets []string
}

// Parse reads a manifest and resolves every secret through getenv.
//
// The order of the returned pools is by provider slug, so a boot log lists them
// the same way twice running.
func Parse(raw []byte, getenv func(string) string) ([]Pool, error) {
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("keyring: the manifest is not valid JSON: %w", err)
	}
	if len(manifest.Providers) == 0 {
		return nil, fmt.Errorf("keyring: the manifest declares no provider, so every route would be unservable")
	}

	slugs := make([]string, 0, len(manifest.Providers))
	for slug := range manifest.Providers {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	pools := make([]Pool, 0, len(slugs))
	for _, slug := range slugs {
		pool, err := resolve(contract.ProviderSlug(slug), manifest.Providers[slug], getenv)
		if err != nil {
			return nil, err
		}
		pools = append(pools, pool)
	}
	return pools, nil
}

func resolve(slug contract.ProviderSlug, config ProviderKeyConfig, getenv func(string) string) (Pool, error) {
	if len(config.Keys) == 0 {
		// A provider entry with no keys reads as configured and serves nothing.
		return Pool{}, fmt.Errorf("keyring: provider %q declares no keys; remove the entry rather than leaving one that holds nothing", slug)
	}

	retirement := time.Duration(0)
	if trimmed := strings.TrimSpace(config.KeyRetirement); trimmed != "" {
		parsed, err := time.ParseDuration(trimmed)
		if err != nil {
			return Pool{}, fmt.Errorf("keyring: provider %q declares keyRetirement %q, which is not a duration: %w", slug, trimmed, err)
		}
		if parsed <= 0 {
			return Pool{}, fmt.Errorf("keyring: provider %q declares keyRetirement %q; a retirement that is not positive returns a spent key immediately", slug, trimmed)
		}
		retirement = parsed
	}

	for name, value := range config.Headers {
		if strings.TrimSpace(name) == "" {
			return Pool{}, fmt.Errorf("keyring: provider %q declares a header with no name", slug)
		}
		if strings.TrimSpace(value) == "" {
			// An empty header is sent and means nothing, so a provider that
			// required it is not satisfied and nothing says so.
			return Pool{}, fmt.Errorf("keyring: provider %q declares header %q with no value", slug, name)
		}
	}

	pool := Pool{
		Provider: slug,
		Protocol: strings.TrimSpace(config.Protocol),
		BaseURL:  strings.TrimSpace(config.BaseURL),
		Headers:  config.Headers,
		Policy:   provider.KeyPolicy{Retirement: retirement, OnSeparateAccounts: config.KeysOnSeparateAccounts},
	}

	for index, key := range config.Keys {
		position := index + 1
		keyID := strings.TrimSpace(key.KeyID)
		if keyID == "" {
			return Pool{}, fmt.Errorf("keyring: provider %q declares a key at position %d with no keyId; it is what names this credential in every log", slug, position)
		}
		variable := strings.TrimSpace(key.SecretEnv)
		inline := strings.TrimSpace(key.Secret)
		switch {
		case variable != "" && inline != "":
			return Pool{}, fmt.Errorf("keyring: provider %q key %q declares both secret and secretEnv; that is two answers to which credential this is, and preferring one silently is how a rotated key keeps serving the old value", slug, keyID)
		case variable == "" && inline == "":
			return Pool{}, fmt.Errorf("keyring: provider %q key %q names neither secret nor secretEnv", slug, keyID)
		}

		secret := inline
		if variable != "" {
			secret = strings.TrimSpace(getenv(variable))
			if secret == "" {
				// Named, because "a credential is missing" sends an operator
				// looking through thirty of them.
				return Pool{}, fmt.Errorf("keyring: provider %q key %q reads %s, which is unset or empty", slug, keyID, variable)
			}
		}

		class := provider.KeyClass(strings.TrimSpace(key.Class))
		switch class {
		case provider.KeyClassFree, provider.KeyClassPaid, provider.KeyClassUnstated:
		default:
			return Pool{}, fmt.Errorf("keyring: provider %q key %q declares class %q; it is %q, %q, or absent", slug, keyID, class, provider.KeyClassFree, provider.KeyClassPaid)
		}

		if key.BudgetUSD != nil {
			pool.UnenforcedBudgets = append(pool.UnenforcedBudgets, keyID)
		}

		pool.Declarations = append(pool.Declarations, provider.KeyDeclaration{KeyID: keyID, Secret: secret, Class: class})
	}
	return pool, nil
}
