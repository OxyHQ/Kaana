package credentialstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

var (
	ErrPlatformCredentialConflict = errors.New("platform credential operation conflicts")
	ErrPlatformCredentialInvalid  = errors.New("platform credential mutation is invalid")
)

type PlatformCredentialMutation struct {
	SchemaVersion  int                   `json:"schemaVersion"`
	OperationID    string                `json:"operationId"`
	Provider       contract.ProviderSlug `json:"provider"`
	KeyID          string                `json:"keyId"`
	SecretBase64   string                `json:"secretBase64"`
	Class          provider.KeyClass     `json:"class"`
	Position       int                   `json:"position"`
	OperationActor string                `json:"operationActor"`
}

type PlatformCredentialReceipt struct {
	OperationID string                `json:"operationId"`
	Provider    contract.ProviderSlug `json:"provider"`
	KeyID       string                `json:"keyId"`
	Outcome     string                `json:"outcome"`
	Replayed    bool                  `json:"replayed"`
}

type PlatformCredentialWrite struct {
	PlatformCredentialMutation
	Ciphertext        []byte
	KMSKeyARN         string
	SecretFingerprint [sha256.Size]byte
}

type platformCredentialRepository interface {
	PutPlatformCredential(context.Context, PlatformCredentialWrite) (PlatformCredentialReceipt, error)
}

type encryptOnlyCipher interface {
	Encrypt(context.Context, Scope, []byte) ([]byte, string, error)
}

type PlatformWriter struct {
	repository platformCredentialRepository
	encryptor  encryptOnlyCipher
}

func NewPlatformWriter(repository platformCredentialRepository, encryptor encryptOnlyCipher) (*PlatformWriter, error) {
	if repository == nil {
		return nil, errors.New("credential store: platform repository is required")
	}
	if encryptor == nil {
		return nil, errors.New("credential store: platform encryptor is required")
	}
	return &PlatformWriter{repository: repository, encryptor: encryptor}, nil
}

func (w *PlatformWriter) Import(ctx context.Context, mutation PlatformCredentialMutation, secret []byte) (PlatformCredentialReceipt, error) {
	if mutation.SchemaVersion != 1 || !validPlatformOperationID(mutation.OperationID) || !mutation.Provider.Valid() ||
		!validOpaqueCredentialID(mutation.KeyID) || mutation.Position < 1 ||
		(mutation.Class != provider.KeyClassFree && mutation.Class != provider.KeyClassPaid) ||
		validateActor(mutation.OperationActor) != nil || provider.ValidateCustomerCredential(secret) != nil {
		return PlatformCredentialReceipt{}, ErrPlatformCredentialInvalid
	}
	scope := Scope{Provider: mutation.Provider, KeyID: mutation.KeyID}
	ciphertext, keyARN, err := w.encryptor.Encrypt(ctx, scope, secret)
	if err != nil {
		clear(ciphertext)
		return PlatformCredentialReceipt{}, err
	}
	defer clear(ciphertext)
	return w.repository.PutPlatformCredential(ctx, PlatformCredentialWrite{
		PlatformCredentialMutation: mutation,
		Ciphertext:                 ciphertext,
		KMSKeyARN:                  keyARN,
		SecretFingerprint:          sha256.Sum256(secret),
	})
}

func validPlatformOperationID(value string) bool {
	if len(value) != 36 || !strings.HasPrefix(value, "kpc_") {
		return false
	}
	for _, character := range value[4:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
