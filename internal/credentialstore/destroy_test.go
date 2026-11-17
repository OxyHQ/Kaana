package credentialstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

// prefixCipher's ciphertext is "<provider>/<key>:" + plaintext, and Decrypt
// refuses any other context, so a test cannot pass by decrypting one row's
// bytes under another identity.
type prefixCipher struct{ decryptions int }

func (c *prefixCipher) Encrypt(_ context.Context, scope credentialstore.Scope, plaintext []byte) ([]byte, string, error) {
	return append([]byte(string(scope.Provider)+"/"+scope.KeyID+":"), plaintext...), testKeyARN, nil
}

func (c *prefixCipher) Decrypt(_ context.Context, scope credentialstore.Scope, ciphertext []byte, keyARN string) ([]byte, error) {
	c.decryptions++
	prefix := []byte(string(scope.Provider) + "/" + scope.KeyID + ":")
	if keyARN != testKeyARN || !bytes.HasPrefix(ciphertext, prefix) {
		return nil, errors.New("context mismatch")
	}
	return append([]byte(nil), ciphertext[len(prefix):]...), nil
}

type destroyRepository struct {
	fakeRepository
	candidate  credentialstore.EncryptedCredential
	references []credentialstore.EncryptedCredential
	decision   *credentialstore.DestructionDecision
}

func (r *destroyRepository) DestroyCredential(_ context.Context, operation credentialstore.DestroyOperation, decide credentialstore.DestructionDecide) (credentialstore.DestructionReceipt, error) {
	decision, err := decide(r.candidate, r.references)
	if err != nil {
		return credentialstore.DestructionReceipt{}, err
	}
	r.decision = &decision
	return credentialstore.DestructionReceipt{
		OperationID: operation.OperationID, Provider: operation.Provider, KeyID: operation.KeyID,
		Proof: decision.Proof, DuplicateOfKeyID: decision.DuplicateOfKeyID,
	}, nil
}

type scriptedProbe struct {
	verdict credentialstore.LivenessVerdict
	err     error
	calls   int
	secret  string
}

func (p *scriptedProbe) Probe(_ context.Context, _ contract.ProviderSlug, secret []byte) (credentialstore.LivenessVerdict, error) {
	p.calls++
	p.secret = string(secret)
	return p.verdict, p.err
}

const destroyOperationID = "kcd_0123456789abcdef0123456789abcdef"

func destroyFixture(t *testing.T, candidateSecret string, enabled map[string]string) (*credentialstore.Store, *destroyRepository, *prefixCipher, credentialstore.DestroyOperation) {
	t.Helper()
	cipher := &prefixCipher{}
	row := func(keyID, secret string) credentialstore.EncryptedCredential {
		scope := credentialstore.Scope{Provider: "elevenlabs", KeyID: keyID}
		ciphertext, arn, _ := cipher.Encrypt(context.Background(), scope, []byte(secret))
		return credentialstore.EncryptedCredential{Scope: scope, Ciphertext: ciphertext, KMSKeyARN: arn}
	}
	repository := &destroyRepository{candidate: row("9b2f6c1e-4d3a-4f7b-8e21-5c0d7a6b3f10", candidateSecret)}
	for keyID, secret := range enabled {
		repository.references = append(repository.references, row(keyID, secret))
	}
	store, err := credentialstore.New(repository, cipher)
	if err != nil {
		t.Fatal(err)
	}
	return store, repository, cipher, credentialstore.DestroyOperation{
		OperationID: destroyOperationID,
		Scope:       credentialstore.Scope{Provider: "elevenlabs", KeyID: "9b2f6c1e-4d3a-4f7b-8e21-5c0d7a6b3f10"},
		Actor:       "operator:test",
	}
}

func TestADuplicateOfAnEnabledKeyIsDestroyedWithoutAskingTheProvider(t *testing.T) {
	store, _, _, operation := destroyFixture(t, "sk-shared", map[string]string{
		"e1ed4df9-3274-4833-8eff-a946d761d36c": "sk-other",
		"6e4abb22-af03-46fb-95d9-b2e4286657f2": "sk-shared",
	})
	probe := &scriptedProbe{verdict: credentialstore.LivenessAccepted}
	receipt, err := store.Destroy(context.Background(), operation, probe)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if receipt.Proof != credentialstore.DestructionProofDuplicateOfEnabled || receipt.DuplicateOfKeyID != "6e4abb22-af03-46fb-95d9-b2e4286657f2" {
		t.Fatalf("receipt = %+v, want a duplicate of the enabled key that holds the same secret", receipt)
	}
	if probe.calls != 0 {
		t.Fatalf("the provider was asked %d times about a secret an enabled key holds", probe.calls)
	}
}

func TestAPrefixOfAnEnabledSecretIsNotADuplicate(t *testing.T) {
	store, _, _, operation := destroyFixture(t, "sk-shared", map[string]string{
		"6e4abb22-af03-46fb-95d9-b2e4286657f2": "sk-shared-but-longer",
	})
	probe := &scriptedProbe{verdict: credentialstore.LivenessUnknown}
	if _, err := store.Destroy(context.Background(), operation, probe); !errors.Is(err, credentialstore.ErrCredentialLivenessUnproven) {
		t.Fatalf("Destroy = %v, want the provider to decide (a prefix is a different secret)", err)
	}
	if probe.calls != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.calls)
	}
}

func TestASecretTheProviderRejectsIsDestroyed(t *testing.T) {
	store, _, _, operation := destroyFixture(t, "sk-dead", map[string]string{
		"e1ed4df9-3274-4833-8eff-a946d761d36c": "sk-live",
	})
	probe := &scriptedProbe{verdict: credentialstore.LivenessRejected}
	receipt, err := store.Destroy(context.Background(), operation, probe)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if receipt.Proof != credentialstore.DestructionProofProviderRejected || receipt.DuplicateOfKeyID != "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if probe.secret != "sk-dead" {
		t.Fatal("the provider was not asked about the candidate's own secret")
	}
}

func TestALiveOrUnprovenSecretIsNeverDestroyed(t *testing.T) {
	for name, probe := range map[string]*scriptedProbe{
		"accepted":    {verdict: credentialstore.LivenessAccepted},
		"unknown":     {verdict: credentialstore.LivenessUnknown},
		"probe error": {verdict: credentialstore.LivenessRejected, err: errors.New("timeout")},
		"empty":       {},
	} {
		t.Run(name, func(t *testing.T) {
			store, repository, _, operation := destroyFixture(t, "sk-alive", nil)
			_, err := store.Destroy(context.Background(), operation, probe)
			want := credentialstore.ErrCredentialLivenessUnproven
			if name == "accepted" {
				want = credentialstore.ErrCredentialStillValid
			}
			if !errors.Is(err, want) {
				t.Fatalf("Destroy = %v, want %v", err, want)
			}
			if repository.decision != nil {
				t.Fatalf("a decision %+v reached the repository", repository.decision)
			}
		})
	}
}

func TestDestroyRefusesMalformedSelectorsBeforeAnyDecryption(t *testing.T) {
	for name, mutate := range map[string]func(*credentialstore.DestroyOperation){
		"kop prefix":  func(o *credentialstore.DestroyOperation) { o.OperationID = "kop_0123456789abcdef0123456789abcdef" },
		"short id":    func(o *credentialstore.DestroyOperation) { o.OperationID = "kcd_0123" },
		"upper hex":   func(o *credentialstore.DestroyOperation) { o.OperationID = "kcd_0123456789ABCDEF0123456789abcdef" },
		"bad slug":    func(o *credentialstore.DestroyOperation) { o.Provider = "Eleven Labs" },
		"blank key":   func(o *credentialstore.DestroyOperation) { o.KeyID = "" },
		"blank actor": func(o *credentialstore.DestroyOperation) { o.Actor = "" },
	} {
		t.Run(name, func(t *testing.T) {
			store, _, cipher, operation := destroyFixture(t, "sk", nil)
			mutate(&operation)
			if _, err := store.Destroy(context.Background(), operation, &scriptedProbe{verdict: credentialstore.LivenessRejected}); err == nil {
				t.Fatal("a malformed destruction was accepted")
			}
			if cipher.decryptions != 0 {
				t.Fatalf("KMS decrypted %d times before the selectors were validated", cipher.decryptions)
			}
		})
	}
}

func TestDestroyRefusesACandidateThatIsNotTheNamedKey(t *testing.T) {
	store, repository, _, operation := destroyFixture(t, "sk-dead", nil)
	repository.candidate.KeyID = "6e4abb22-af03-46fb-95d9-b2e4286657f2"
	if _, err := store.Destroy(context.Background(), operation, &scriptedProbe{verdict: credentialstore.LivenessRejected}); err == nil {
		t.Fatal("destroyed a key the operation did not name")
	}
}
