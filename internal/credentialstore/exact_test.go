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
)

const exactSecretMarker = "sk-or-v1-EXACT-SECRET-MARKER"

var exactScope = credentialstore.Scope{Provider: "openrouter", KeyID: "openrouter-exact-key"}

// exactRepository serves one row by exact identity and fails the test if the
// pool-wide listing is ever used.
type exactRepository struct {
	*fakeRepository
	t         *testing.T
	row       credentialstore.EncryptedCredential
	err       error
	requested []credentialstore.Scope
}

func (r *exactRepository) ListEnabled(context.Context, []contract.ProviderSlug) ([]credentialstore.EncryptedCredential, error) {
	r.t.Error("exact custody listed the provider pool")
	return nil, errors.New("pool listing is not allowed")
}

func (r *exactRepository) GetEnabled(_ context.Context, scope credentialstore.Scope) (credentialstore.EncryptedCredential, error) {
	r.requested = append(r.requested, scope)
	return r.row, r.err
}

// recordingCipher hands out plaintext it keeps, so the test can prove the
// buffer was cleared after WithCredential returned.
type recordingCipher struct {
	plaintext []byte
	err       error
	scopes    []credentialstore.Scope
}

func (c *recordingCipher) Encrypt(context.Context, credentialstore.Scope, []byte) ([]byte, string, error) {
	return nil, "", errors.New("encrypt is not used by exact custody")
}

func (c *recordingCipher) Decrypt(_ context.Context, scope credentialstore.Scope, _ []byte, _ string) ([]byte, error) {
	c.scopes = append(c.scopes, scope)
	return c.plaintext, c.err
}

func exactRow(scope credentialstore.Scope) credentialstore.EncryptedCredential {
	return credentialstore.EncryptedCredential{Scope: scope, Ciphertext: []byte("ciphertext"), KMSKeyARN: testKeyARN, Position: 1}
}

func newExactStore(t *testing.T, repository credentialstore.Repository, cipher credentialstore.Cipher) *credentialstore.Store {
	t.Helper()
	store, err := credentialstore.New(repository, cipher)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertCleared(t *testing.T, buffer []byte) {
	t.Helper()
	if len(buffer) == 0 {
		t.Fatal("test cipher returned no buffer to inspect")
	}
	if !bytes.Equal(buffer, make([]byte, len(buffer))) {
		t.Fatal("plaintext buffer was not cleared")
	}
}

func assertNoMarker(t *testing.T, err error) {
	t.Helper()
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), exactSecretMarker) || strings.Contains(current.Error(), exactScope.KeyID) {
			t.Fatalf("error chain exposes a secret or identifier: %q", current)
		}
	}
}

func TestWithCredentialLendsExactlyTheRequestedKeyAndClearsIt(t *testing.T) {
	repository := &exactRepository{fakeRepository: &fakeRepository{}, t: t, row: exactRow(exactScope)}
	cipher := &recordingCipher{plaintext: []byte(exactSecretMarker)}
	store := newExactStore(t, repository, cipher)

	var seen string
	if err := store.WithCredential(context.Background(), exactScope, func(secret []byte) error {
		seen = string(secret)
		return nil
	}); err != nil {
		t.Fatalf("WithCredential: %v", err)
	}
	if seen != exactSecretMarker {
		t.Fatal("callback did not receive the decrypted credential")
	}
	if len(repository.requested) != 1 || repository.requested[0] != exactScope {
		t.Fatalf("repository requests = %v, want exactly the requested scope", repository.requested)
	}
	if len(cipher.scopes) != 1 || cipher.scopes[0] != exactScope {
		t.Fatalf("decrypt scopes = %v, want exactly the requested scope", cipher.scopes)
	}
	assertCleared(t, cipher.plaintext)
}

func TestWithCredentialRefusesAnotherRowWithoutDecrypting(t *testing.T) {
	for name, row := range map[string]credentialstore.EncryptedCredential{
		"other key id":   exactRow(credentialstore.Scope{Provider: "openrouter", KeyID: "openrouter-other-key"}),
		"other provider": exactRow(credentialstore.Scope{Provider: "groq", KeyID: exactScope.KeyID}),
		"no ciphertext":  {Scope: exactScope, KMSKeyARN: testKeyARN, Position: 1},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &exactRepository{fakeRepository: &fakeRepository{}, t: t, row: row}
			cipher := &recordingCipher{plaintext: []byte(exactSecretMarker)}
			called := false
			err := newExactStore(t, repository, cipher).WithCredential(context.Background(), exactScope, func([]byte) error {
				called = true
				return nil
			})
			if !errors.Is(err, credentialstore.ErrCredentialUnavailable) {
				t.Fatalf("err = %v, want ErrCredentialUnavailable", err)
			}
			if called || len(cipher.scopes) != 0 {
				t.Fatal("a row other than the exact valid credential was decrypted or lent")
			}
		})
	}
}

func TestWithCredentialDoesNotFallBackWhenTheExactKeyIsAbsent(t *testing.T) {
	repository := &exactRepository{
		fakeRepository: &fakeRepository{rows: []credentialstore.EncryptedCredential{exactRow(credentialstore.Scope{Provider: "openrouter", KeyID: "pool-fallback"})}},
		t:              t,
		err:            fmt.Errorf("%w: %s", credentialstore.ErrCredentialNotActive, exactSecretMarker),
	}
	cipher := &recordingCipher{plaintext: []byte(exactSecretMarker)}
	err := newExactStore(t, repository, cipher).WithCredential(context.Background(), exactScope, func([]byte) error {
		t.Fatal("callback ran without the exact credential")
		return nil
	})
	if !errors.Is(err, credentialstore.ErrCredentialNotActive) {
		t.Fatalf("err = %v, want the bare ErrCredentialNotActive sentinel", err)
	}
	if len(cipher.scopes) != 0 {
		t.Fatal("decrypt ran for an absent exact credential")
	}
	assertNoMarker(t, err)
}

func TestWithCredentialErrorsRetainNoUnderlyingChain(t *testing.T) {
	leak := errors.New("postgres://admin:" + exactSecretMarker + "@db/" + exactScope.KeyID)
	cases := map[string]struct {
		repositoryErr error
		decryptErr    error
		plaintext     []byte
		callbackErr   error
		want          error
	}{
		"database error":   {repositoryErr: leak, want: credentialstore.ErrCredentialUnavailable},
		"kms error":        {decryptErr: leak, plaintext: []byte(exactSecretMarker), want: credentialstore.ErrCredentialUnavailable},
		"invalid secret":   {plaintext: []byte("has space " + exactSecretMarker), want: credentialstore.ErrCredentialUnavailable},
		"callback error":   {plaintext: []byte(exactSecretMarker), callbackErr: leak, want: credentialstore.ErrCredentialUseFailed},
		"wrapped callback": {plaintext: []byte(exactSecretMarker), callbackErr: fmt.Errorf("%w: %w", credentialstore.ErrCredentialNotActive, leak), want: credentialstore.ErrCredentialUseFailed},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			repository := &exactRepository{fakeRepository: &fakeRepository{}, t: t, row: exactRow(exactScope), err: test.repositoryErr}
			cipher := &recordingCipher{plaintext: test.plaintext, err: test.decryptErr}
			err := newExactStore(t, repository, cipher).WithCredential(context.Background(), exactScope, func([]byte) error {
				return test.callbackErr
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want the bare %v sentinel", err, test.want)
			}
			if errors.Unwrap(err) != nil {
				t.Fatal("returned error retains a wrapped chain")
			}
			assertNoMarker(t, err)
			if test.plaintext != nil {
				assertCleared(t, cipher.plaintext)
			}
		})
	}
}

func TestWithCredentialRefusesInvalidRequestsBeforeTheRepository(t *testing.T) {
	repository := &exactRepository{fakeRepository: &fakeRepository{}, t: t, row: exactRow(exactScope)}
	store := newExactStore(t, repository, &recordingCipher{plaintext: []byte(exactSecretMarker)})
	for name, scope := range map[string]credentialstore.Scope{
		"empty key":    {Provider: "openrouter"},
		"padded key":   {Provider: "openrouter", KeyID: " " + exactScope.KeyID},
		"bad provider": {Provider: "Open Router", KeyID: exactScope.KeyID},
	} {
		t.Run(name, func(t *testing.T) {
			err := store.WithCredential(context.Background(), scope, func([]byte) error { return nil })
			if !errors.Is(err, credentialstore.ErrCredentialUnavailable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if err := store.WithCredential(context.Background(), exactScope, nil); !errors.Is(err, credentialstore.ErrCredentialUnavailable) {
		t.Fatalf("nil callback err = %v", err)
	}
	if len(repository.requested) != 0 {
		t.Fatal("an invalid request reached the repository")
	}
}

func TestWithCredentialRequiresAnExactRepository(t *testing.T) {
	// Negative control: the plain fake can list a pool but has no exact read,
	// and custody must refuse rather than choose from that pool.
	repository := &fakeRepository{rows: []credentialstore.EncryptedCredential{exactRow(exactScope)}}
	cipher := &recordingCipher{plaintext: []byte(exactSecretMarker)}
	err := newExactStore(t, repository, cipher).WithCredential(context.Background(), exactScope, func([]byte) error { return nil })
	if !errors.Is(err, credentialstore.ErrCredentialUnavailable) || len(cipher.scopes) != 0 {
		t.Fatalf("err = %v, decrypts = %d", err, len(cipher.scopes))
	}
}
