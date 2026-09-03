package credentialstore_test

import (
	"bytes"
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
	rows              []credentialstore.EncryptedCredential
	put               *credentialstore.EncryptedCredential
	disabled          credentialstore.Scope
	actor             string
	rekeySource       credentialstore.EncryptedCredential
	rekeyDestination  credentialstore.EncryptedCredential
	deduplicateKeep   credentialstore.EncryptedCredential
	deduplicateSource credentialstore.EncryptedCredential
	deduplicateEqual  bool
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

func (f *fakeRepository) RekeyID(_ context.Context, operation credentialstore.CredentialIDOperation, transform credentialstore.CredentialRekeyTransform) (credentialstore.CredentialAdminReceipt, error) {
	destination, err := transform(f.rekeySource)
	if err != nil {
		return credentialstore.CredentialAdminReceipt{}, err
	}
	f.rekeyDestination = destination
	return credentialstore.CredentialAdminReceipt{
		OperationID: operation.OperationID, Action: credentialstore.CredentialAdminActionRekeyID,
		Provider: operation.Provider, SourceKeyID: operation.SourceKeyID,
		DestinationKeyID: operation.DestinationKeyID,
		Outcome:          credentialstore.CredentialAdminOutcomeApplied,
	}, nil
}

func (f *fakeRepository) Deduplicate(_ context.Context, operation credentialstore.CredentialIDOperation, compare credentialstore.CredentialCompareTransform) (credentialstore.CredentialAdminReceipt, error) {
	equal, err := compare(f.deduplicateKeep, f.deduplicateSource)
	if err != nil {
		return credentialstore.CredentialAdminReceipt{}, err
	}
	f.deduplicateEqual = equal
	outcome := credentialstore.CredentialAdminOutcomeDifferent
	if equal {
		outcome = credentialstore.CredentialAdminOutcomeDeduplicated
	}
	return credentialstore.CredentialAdminReceipt{
		OperationID: operation.OperationID, Action: credentialstore.CredentialAdminActionDeduplicate,
		Provider: operation.Provider, SourceKeyID: operation.SourceKeyID,
		DestinationKeyID: operation.DestinationKeyID, Outcome: outcome,
	}, nil
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

type plaintextErrorCipher struct {
	outputs map[string][]byte
}

func (p *plaintextErrorCipher) Encrypt(context.Context, credentialstore.Scope, []byte) ([]byte, string, error) {
	return []byte("partial-ciphertext"), "", errors.New("encrypt failed")
}

func (p *plaintextErrorCipher) Decrypt(_ context.Context, scope credentialstore.Scope, _ []byte, _ string) ([]byte, error) {
	output := []byte("plaintext-on-error")
	p.outputs[scope.KeyID] = output
	return output, errors.New("decrypt failed")
}

type encryptionErrorCipher struct {
	plaintext  []byte
	ciphertext []byte
}

type decryptDeniedCipher struct {
	decryptCalls int
	encryptCalls int
}

func (c *decryptDeniedCipher) Encrypt(context.Context, credentialstore.Scope, []byte) ([]byte, string, error) {
	c.encryptCalls++
	return nil, "", errors.New("unexpected encrypt after denied decrypt")
}

func (c *decryptDeniedCipher) Decrypt(context.Context, credentialstore.Scope, []byte, string) ([]byte, error) {
	c.decryptCalls++
	return nil, errors.New("AccessDeniedException: kms:Decrypt")
}

func (e *encryptionErrorCipher) Encrypt(_ context.Context, _ credentialstore.Scope, plaintext []byte) ([]byte, string, error) {
	e.plaintext = plaintext
	e.ciphertext = []byte("partial-ciphertext")
	return e.ciphertext, "", errors.New("encrypt failed")
}

func (e *encryptionErrorCipher) Decrypt(context.Context, credentialstore.Scope, []byte, string) ([]byte, error) {
	return []byte("plaintext-before-encrypt-error"), nil
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
		"non-exact decrypted credential": {
			row("groq", "first", 1, provider.KeyClassFree, nil, " secret"),
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
		Scope:    credentialstore.Scope{Provider: "groq", KeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1"},
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
	if repository.put.KeyID != "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1" || repository.put.Provider != "groq" {
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
		Scope:    credentialstore.Scope{Provider: "groq", KeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1"},
		Position: 1,
	}
	if err := store.Put(context.Background(), input, []byte("secret"), ""); err == nil {
		t.Fatal("put without an audit actor was accepted")
	}
	if _, err := store.Disable(context.Background(), input.Scope, " actor-with-whitespace "); err == nil {
		t.Fatal("disable with an unsafe audit actor was accepted")
	}
}

func TestPutRejectsNonExactCredentialAndRowIdentities(t *testing.T) {
	store, err := credentialstore.New(&fakeRepository{}, opaqueCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := credentialstore.EncryptedCredential{
		Scope:    credentialstore.Scope{Provider: "groq", KeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1"},
		Position: 1,
	}
	for name, mutate := range map[string]func(*credentialstore.EncryptedCredential, *[]byte){
		"provider whitespace": func(input *credentialstore.EncryptedCredential, _ *[]byte) { input.Provider = " groq" },
		"key id whitespace":   func(input *credentialstore.EncryptedCredential, _ *[]byte) { input.KeyID = " primary" },
		"class whitespace":    func(input *credentialstore.EncryptedCredential, _ *[]byte) { input.Class = " paid" },
		"secret whitespace":   func(_ *credentialstore.EncryptedCredential, secret *[]byte) { *secret = []byte(" secret") },
		"secret control":      func(_ *credentialstore.EncryptedCredential, secret *[]byte) { *secret = []byte("secret\tvalue") },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			secret := []byte("secret")
			mutate(&input, &secret)
			if err := store.Put(context.Background(), input, secret, "operator:test"); err == nil {
				t.Fatal("non-exact credential input was accepted")
			}
		})
	}
}

func TestPutRequiresOpaqueIDsAndLegacyImportIsAnExactException(t *testing.T) {
	store, err := credentialstore.New(&fakeRepository{}, opaqueCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input := credentialstore.EncryptedCredential{
		Scope:    credentialstore.Scope{Provider: "groq", KeyID: "groq-primary"},
		Position: 1,
	}
	if err := store.Put(context.Background(), input, []byte("secret"), "operator:test"); err == nil {
		t.Fatal("a new semantic key id was accepted")
	}
	input.KeyID = "legacy-alia-20260901"
	if err := store.ImportLegacy(context.Background(), input, []byte("secret"), "operator:test"); err != nil {
		t.Fatalf("exact historical import: %v", err)
	}
	input.KeyID = "another-legacy-name"
	if err := store.ImportLegacy(context.Background(), input, []byte("secret"), "operator:test"); err == nil {
		t.Fatal("an unreviewed legacy identity was accepted")
	}
}

func TestRekeyIDChangesOnlyTheExactKMSContextAndPreservesMetadata(t *testing.T) {
	budget := 12.5
	repository := &fakeRepository{rekeySource: row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, &budget, "provider-secret")}
	store, err := credentialstore.New(repository, contextualCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	operation := credentialstore.CredentialIDOperation{
		OperationID: "kop_0af8007d9fdddd88d2622eabff99aeb9",
		Provider:    "groq", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1",
		Actor:            "operator:test",
	}
	receipt, err := store.RekeyID(context.Background(), operation)
	if err != nil {
		t.Fatalf("RekeyID: %v", err)
	}
	if receipt.Outcome != credentialstore.CredentialAdminOutcomeApplied {
		t.Fatalf("receipt = %+v", receipt)
	}
	destination := repository.rekeyDestination
	if destination.Provider != operation.Provider || destination.KeyID != operation.DestinationKeyID ||
		destination.Position != 1 || destination.Class != provider.KeyClassPaid || destination.BudgetUSD == nil || *destination.BudgetUSD != budget {
		t.Fatalf("destination metadata = %+v", destination)
	}
	expectedCiphertext := "groq/8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1:provider-secret"
	if string(destination.Ciphertext) != expectedCiphertext {
		t.Fatalf("destination ciphertext = %q", destination.Ciphertext)
	}
}

func TestDeduplicateComparesExactIDsWithoutASecretDigest(t *testing.T) {
	for name, candidate := range map[string]string{
		"equal":     "provider-secret",
		"different": "another-secret",
	} {
		t.Run(name, func(t *testing.T) {
			repository := &fakeRepository{
				deduplicateKeep:   row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, nil, "provider-secret"),
				deduplicateSource: row("groq", "relay-groq-20260902", 2, provider.KeyClassPaid, nil, candidate),
			}
			store, err := credentialstore.New(repository, contextualCipher{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			operation := credentialstore.CredentialIDOperation{
				OperationID: "kop_3ac18ed3ab6c6bf97862b03193ef4357",
				Provider:    "groq", SourceKeyID: "relay-groq-20260902",
				DestinationKeyID: "legacy-alia-20260901", Actor: "operator:test",
			}
			receipt, err := store.Deduplicate(context.Background(), operation)
			if err != nil {
				t.Fatalf("Deduplicate: %v", err)
			}
			if repository.deduplicateEqual != (name == "equal") {
				t.Fatalf("comparison result = %v", repository.deduplicateEqual)
			}
			expected := credentialstore.CredentialAdminOutcomeDifferent
			if name == "equal" {
				expected = credentialstore.CredentialAdminOutcomeDeduplicated
			}
			if receipt.Outcome != expected {
				t.Fatalf("receipt = %+v", receipt)
			}
		})
	}
}

func TestCredentialIDOperationsRequireExactOpaqueIdentities(t *testing.T) {
	store, err := credentialstore.New(&fakeRepository{}, contextualCipher{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := credentialstore.CredentialIDOperation{
		OperationID: "kop_5b4f96c394a7a288754a1388fed0c5b2",
		Provider:    "groq", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1",
		Actor:            "operator:test",
	}
	for name, mutate := range map[string]func(*credentialstore.CredentialIDOperation){
		"operation whitespace": func(input *credentialstore.CredentialIDOperation) { input.OperationID += " " },
		"operation is named":   func(input *credentialstore.CredentialIDOperation) { input.OperationID = "rename-groq" },
		"provider whitespace":  func(input *credentialstore.CredentialIDOperation) { input.Provider = " groq" },
		"source whitespace":    func(input *credentialstore.CredentialIDOperation) { input.SourceKeyID += " " },
		"destination uppercase": func(input *credentialstore.CredentialIDOperation) {
			input.DestinationKeyID = strings.ToUpper(input.DestinationKeyID)
		},
		"destination named": func(input *credentialstore.CredentialIDOperation) { input.DestinationKeyID = "groq-main" },
		"same IDs":          func(input *credentialstore.CredentialIDOperation) { input.DestinationKeyID = input.SourceKeyID },
		"actor newline":     func(input *credentialstore.CredentialIDOperation) { input.Actor = "operator:test\nforged" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := store.RekeyID(context.Background(), input); err == nil {
				t.Fatal("non-exact rekey operation was accepted")
			}
		})
	}
}

func TestCredentialIDOperationsClearPlaintextReturnedWithAnError(t *testing.T) {
	cipher := &plaintextErrorCipher{outputs: make(map[string][]byte)}
	repository := &fakeRepository{
		rekeySource:       row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, nil, "not-used"),
		deduplicateKeep:   row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, nil, "not-used"),
		deduplicateSource: row("groq", "relay-groq-20260902", 2, provider.KeyClassPaid, nil, "not-used"),
	}
	store, err := credentialstore.New(repository, cipher)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rekey := credentialstore.CredentialIDOperation{
		OperationID: "kop_5b4f96c394a7a288754a1388fed0c5b2",
		Provider:    "groq", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1", Actor: "operator:test",
	}
	if _, err := store.RekeyID(context.Background(), rekey); err == nil {
		t.Fatal("rekey accepted a decrypt error")
	}
	for keyID, output := range cipher.outputs {
		if !bytes.Equal(output, make([]byte, len(output))) {
			t.Fatalf("decrypt error plaintext for %q was not cleared: %q", keyID, output)
		}
	}

	cipher.outputs = make(map[string][]byte)
	deduplicate := credentialstore.CredentialIDOperation{
		OperationID: "kop_0af8007d9fdddd88d2622eabff99aeb9",
		Provider:    "groq", SourceKeyID: "relay-groq-20260902",
		DestinationKeyID: "legacy-alia-20260901", Actor: "operator:test",
	}
	if _, err := store.Deduplicate(context.Background(), deduplicate); err == nil {
		t.Fatal("deduplication accepted a decrypt error")
	}
	for keyID, output := range cipher.outputs {
		if !bytes.Equal(output, make([]byte, len(output))) {
			t.Fatalf("deduplication decrypt error plaintext for %q was not cleared: %q", keyID, output)
		}
	}
}

func TestCredentialIDOperationsRequireKMSDecryptBeforeAnyMutation(t *testing.T) {
	rekeyRepository := &fakeRepository{
		rekeySource: row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, nil, "not-exposed"),
	}
	rekeyCipher := &decryptDeniedCipher{}
	rekeyStore, err := credentialstore.New(rekeyRepository, rekeyCipher)
	if err != nil {
		t.Fatalf("New rekey store: %v", err)
	}
	rekey := credentialstore.CredentialIDOperation{
		OperationID: "kop_5b4f96c394a7a288754a1388fed0c5b2",
		Provider:    "groq", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1", Actor: "operator:test",
	}
	if _, err := rekeyStore.RekeyID(context.Background(), rekey); err == nil || !strings.Contains(err.Error(), "kms:Decrypt") {
		t.Fatalf("rekey without kms:Decrypt = %v", err)
	}
	if rekeyCipher.decryptCalls != 1 || rekeyCipher.encryptCalls != 0 || rekeyRepository.rekeyDestination.KeyID != "" {
		t.Fatalf("denied rekey decrypt calls=%d encrypt calls=%d destination=%+v", rekeyCipher.decryptCalls, rekeyCipher.encryptCalls, rekeyRepository.rekeyDestination.Scope)
	}

	deduplicateRepository := &fakeRepository{
		deduplicateKeep:   row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, nil, "not-exposed"),
		deduplicateSource: row("groq", "relay-groq-20260902", 2, provider.KeyClassPaid, nil, "not-exposed"),
	}
	deduplicateCipher := &decryptDeniedCipher{}
	deduplicateStore, err := credentialstore.New(deduplicateRepository, deduplicateCipher)
	if err != nil {
		t.Fatalf("New deduplication store: %v", err)
	}
	deduplicate := credentialstore.CredentialIDOperation{
		OperationID: "kop_0af8007d9fdddd88d2622eabff99aeb9",
		Provider:    "groq", SourceKeyID: "relay-groq-20260902",
		DestinationKeyID: "legacy-alia-20260901", Actor: "operator:test",
	}
	if _, err := deduplicateStore.Deduplicate(context.Background(), deduplicate); err == nil || !strings.Contains(err.Error(), "kms:Decrypt") {
		t.Fatalf("deduplication without kms:Decrypt = %v", err)
	}
	if deduplicateCipher.decryptCalls != 1 || deduplicateCipher.encryptCalls != 0 || deduplicateRepository.deduplicateEqual {
		t.Fatalf("denied deduplication decrypt calls=%d encrypt calls=%d equality=%v", deduplicateCipher.decryptCalls, deduplicateCipher.encryptCalls, deduplicateRepository.deduplicateEqual)
	}
}

func TestRekeyClearsPlaintextAndPartialCiphertextAfterEncryptionError(t *testing.T) {
	cipher := &encryptionErrorCipher{}
	repository := &fakeRepository{
		rekeySource: row("groq", "legacy-alia-20260901", 1, provider.KeyClassPaid, nil, "not-used"),
	}
	store, err := credentialstore.New(repository, cipher)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	operation := credentialstore.CredentialIDOperation{
		OperationID: "kop_5b4f96c394a7a288754a1388fed0c5b2",
		Provider:    "groq", SourceKeyID: "legacy-alia-20260901",
		DestinationKeyID: "8295090b-86cf-4f1d-ab22-0ceeaf0ba0e1", Actor: "operator:test",
	}
	if _, err := store.RekeyID(context.Background(), operation); err == nil {
		t.Fatal("rekey accepted an encryption failure")
	}
	for label, output := range map[string][]byte{"plaintext": cipher.plaintext, "partial ciphertext": cipher.ciphertext} {
		if !bytes.Equal(output, make([]byte, len(output))) {
			t.Fatalf("%s was not cleared: %q", label, output)
		}
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
