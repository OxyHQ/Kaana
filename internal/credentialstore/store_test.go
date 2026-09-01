package credentialstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const testKeyARN = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000001"

type fakeRepository struct {
	rows     []credentialstore.EncryptedCredential
	put      *credentialstore.EncryptedCredential
	disabled credentialstore.Scope
	actor    string
}

func (f *fakeRepository) ListEnabled(context.Context, []contract.ProviderSlug) ([]credentialstore.EncryptedCredential, error) {
	return append([]credentialstore.EncryptedCredential(nil), f.rows...), nil
}

func (f *fakeRepository) Put(_ context.Context, row credentialstore.EncryptedCredential, actor string) error {
	copyOfRow := row
	copyOfRow.Ciphertext = append([]byte(nil), row.Ciphertext...)
	f.put = &copyOfRow
	f.actor = actor
	return nil
}

func (f *fakeRepository) Disable(_ context.Context, scope credentialstore.Scope, actor string) (bool, error) {
	f.disabled = scope
	f.actor = actor
	return true, nil
}

func (f *fakeRepository) ListMetadata(context.Context) ([]credentialstore.Metadata, error) {
	return nil, nil
}

type contextualCipher struct{}

func (contextualCipher) Encrypt(_ context.Context, scope credentialstore.Scope, plaintext []byte) ([]byte, string, error) {
	return []byte(fmt.Sprintf("%s/%s:%s", scope.Provider, scope.KeyID, plaintext)), testKeyARN, nil
}

type opaqueCipher struct{}

func (opaqueCipher) Encrypt(context.Context, credentialstore.Scope, []byte) ([]byte, string, error) {
	return []byte("opaque-ciphertext"), testKeyARN, nil
}

func (opaqueCipher) Decrypt(context.Context, credentialstore.Scope, []byte, string) ([]byte, error) {
	return nil, errors.New("not used")
}

func (contextualCipher) Decrypt(_ context.Context, scope credentialstore.Scope, ciphertext []byte, keyARN string) ([]byte, error) {
	if keyARN != testKeyARN {
		return nil, errors.New("wrong KMS key")
	}
	prefix := fmt.Sprintf("%s/%s:", scope.Provider, scope.KeyID)
	encoded := string(ciphertext)
	if !strings.HasPrefix(encoded, prefix) {
		return nil, errors.New("encryption context does not match the row")
	}
	return []byte(strings.TrimPrefix(encoded, prefix)), nil
}

func TestLoadReturnsOrderedProviderPools(t *testing.T) {
	budget := 10.0
	repository := &fakeRepository{rows: []credentialstore.EncryptedCredential{
		row("openrouter", "paid", 2, provider.KeyClassPaid, &budget, "paid-secret"),
		row("groq", "primary", 1, provider.KeyClassFree, nil, "groq-secret"),
		row("openrouter", "free", 1, provider.KeyClassFree, nil, "free-secret"),
	}}
	store, err := credentialstore.New(repository, contextualCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pools, budgets, err := store.Load(context.Background(), []contract.ProviderSlug{"openrouter", "groq"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := pools["openrouter"]; len(got) != 2 || got[0].KeyID != "free" || got[1].KeyID != "paid" {
		t.Fatalf("openrouter pool = %+v", got)
	}
	if pools["groq"][0].Secret != "groq-secret" {
		t.Error("groq plaintext was not delivered to its provider declaration")
	}
	if len(budgets) != 1 || budgets[0] != "openrouter/paid" {
		t.Fatalf("budgets = %v", budgets)
	}
}

func TestLoadRefusesMissingAndAmbiguousCredentials(t *testing.T) {
	cases := map[string][]credentialstore.EncryptedCredential{
		"missing pool": nil,
		"duplicate positions": {
			row("groq", "first", 1, provider.KeyClassFree, nil, "one"),
			row("groq", "second", 1, provider.KeyClassFree, nil, "two"),
		},
		"ciphertext moved to another row": {
			row("groq", "first", 1, provider.KeyClassFree, nil, "one"),
		},
	}
	// Replace the last row's bound context so it cannot decrypt under its DB
	// identity. This is the row-swap attack KMS encryption context blocks.
	cases["ciphertext moved to another row"][0].Ciphertext = []byte("openrouter/first:one")

	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			store, err := credentialstore.New(&fakeRepository{rows: rows}, contextualCipher{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, _, err := store.Load(context.Background(), []contract.ProviderSlug{"groq"}); err == nil {
				t.Fatal("invalid credential rows were accepted")
			}
		})
	}
}

func TestPutEncryptsBeforeWritingPostgres(t *testing.T) {
	repository := &fakeRepository{}
	store, err := credentialstore.New(repository, opaqueCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plaintext := []byte("provider-secret")
	input := credentialstore.EncryptedCredential{
		Scope:    credentialstore.Scope{Provider: "groq", KeyID: "primary"},
		Class:    provider.KeyClassPaid,
		Position: 1,
	}
	if err := store.Put(context.Background(), input, plaintext, "operator:test"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if repository.put == nil {
		t.Fatal("repository received no row")
	}
	if strings.Contains(string(repository.put.Ciphertext), "provider-secret") {
		t.Fatal("repository received plaintext instead of ciphertext")
	}
	if string(repository.put.Ciphertext) != "opaque-ciphertext" {
		t.Fatalf("repository received %q, want the cipher output", repository.put.Ciphertext)
	}
	if repository.put.KMSKeyARN != testKeyARN {
		t.Fatalf("KMS key ARN = %q", repository.put.KMSKeyARN)
	}
	if repository.put.KeyID != "primary" || repository.put.Provider != "groq" {
		t.Fatalf("row identity changed: %+v", repository.put.Scope)
	}
	if repository.actor != "operator:test" {
		t.Fatalf("mutation actor = %q", repository.actor)
	}
}

func TestMutationsRequireAnAuditActor(t *testing.T) {
	store, err := credentialstore.New(&fakeRepository{}, opaqueCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input := credentialstore.EncryptedCredential{
		Scope:    credentialstore.Scope{Provider: "groq", KeyID: "primary"},
		Position: 1,
	}
	if err := store.Put(context.Background(), input, []byte("secret"), ""); err == nil {
		t.Fatal("put without an audit actor was accepted")
	}
	if _, err := store.Disable(context.Background(), input.Scope, " actor-with-whitespace "); err == nil {
		t.Fatal("disable with an unsafe audit actor was accepted")
	}
}

func row(slug contract.ProviderSlug, keyID string, position int, class provider.KeyClass, budget *float64, secret string) credentialstore.EncryptedCredential {
	return credentialstore.EncryptedCredential{
		Scope:      credentialstore.Scope{Provider: slug, KeyID: keyID},
		Ciphertext: []byte(fmt.Sprintf("%s/%s:%s", slug, keyID, secret)),
		KMSKeyARN:  testKeyARN,
		Class:      class,
		BudgetUSD:  budget,
		Position:   position,
	}
}
