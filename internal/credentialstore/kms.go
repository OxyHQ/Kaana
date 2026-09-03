package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

const (
	contextProvider             = "kaana:provider"
	contextKeyID                = "kaana:key-id"
	contextOwnerAccountID       = "kaana:owner-account-id"
	contextConnectionID         = "kaana:connection-id"
	contextEnvironment          = "kaana:environment"
	contextCredentialHandle     = "kaana:credential-handle"
	contextCredentialRevision   = "kaana:credential-revision"
	contextCredentialClass      = "kaana:credential-class"
	customerCredentialClassBYOK = "customer_byok"
)

type kmsClient interface {
	Encrypt(context.Context, *kms.EncryptInput, ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(context.Context, *kms.DecryptInput, ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// KMSCipher encrypts and decrypts with one explicitly configured symmetric
// KMS key. The ARN is configuration, not secret material.
type KMSCipher struct {
	client kmsClient
	keyARN string
}

// OpenKMSCipher resolves AWS workload credentials and pins the expected key.
func OpenKMSCipher(ctx context.Context, keyARN string) (*KMSCipher, error) {
	if keyARN == "" || keyARN != strings.TrimSpace(keyARN) {
		return nil, errors.New("credential store: KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN is required")
	}
	config, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential store: loading AWS configuration: %w", err)
	}
	return NewKMSCipher(kms.NewFromConfig(config), keyARN)
}

// NewKMSCipher builds the KMS boundary from an explicit client.
func NewKMSCipher(client kmsClient, keyARN string) (*KMSCipher, error) {
	if client == nil {
		return nil, errors.New("credential store: KMS client is required")
	}
	if keyARN == "" || keyARN != strings.TrimSpace(keyARN) {
		return nil, errors.New("credential store: KMS key ARN is required")
	}
	return &KMSCipher{client: client, keyARN: keyARN}, nil
}

// Encrypt returns only ciphertext and the canonical key ARN reported by KMS.
func (c *KMSCipher) Encrypt(ctx context.Context, scope Scope, plaintext []byte) ([]byte, string, error) {
	output, err := c.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:               &c.keyARN,
		Plaintext:           plaintext,
		EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   encryptionContext(scope),
	})
	if err != nil {
		return nil, "", err
	}
	if len(output.CiphertextBlob) == 0 || output.KeyId == nil || *output.KeyId == "" {
		return nil, "", errors.New("KMS returned incomplete ciphertext metadata")
	}
	if *output.KeyId != c.keyARN {
		return nil, "", fmt.Errorf("KMS encrypted with %q, expected %q", *output.KeyId, c.keyARN)
	}
	return output.CiphertextBlob, *output.KeyId, nil
}

// Decrypt refuses a row naming another KMS key before asking AWS to decrypt.
func (c *KMSCipher) Decrypt(ctx context.Context, scope Scope, ciphertext []byte, storedKeyARN string) ([]byte, error) {
	if storedKeyARN != c.keyARN {
		return nil, fmt.Errorf("row names KMS key %q, expected %q", storedKeyARN, c.keyARN)
	}
	output, err := c.client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:      ciphertext,
		KeyId:               &c.keyARN,
		EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   encryptionContext(scope),
	})
	if err != nil {
		if output != nil {
			clear(output.Plaintext)
		}
		return nil, err
	}
	if output == nil {
		return nil, errors.New("KMS returned no decryption result")
	}
	if output.KeyId == nil || *output.KeyId != c.keyARN {
		clear(output.Plaintext)
		return nil, errors.New("KMS decrypted with an unexpected key")
	}
	return output.Plaintext, nil
}

// EncryptCustomer binds a BYOK secret to the complete exact identity Oxy
// authorized, plus Kaana's opaque handle and monotonic revision. A ciphertext
// copied to another owner, connection, provider, environment, handle, or older
// revision is therefore not decryptable there.
func (c *KMSCipher) EncryptCustomer(ctx context.Context, scope CustomerCredentialScope, plaintext []byte) ([]byte, string, error) {
	output, err := c.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:               &c.keyARN,
		Plaintext:           plaintext,
		EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   customerEncryptionContext(scope),
	})
	if err != nil {
		return nil, "", err
	}
	if len(output.CiphertextBlob) == 0 || output.KeyId == nil || *output.KeyId == "" {
		return nil, "", errors.New("KMS returned incomplete ciphertext metadata")
	}
	if *output.KeyId != c.keyARN {
		return nil, "", fmt.Errorf("KMS encrypted with %q, expected %q", *output.KeyId, c.keyARN)
	}
	return output.CiphertextBlob, *output.KeyId, nil
}

// DecryptCustomer refuses a row naming another KMS key before asking AWS.
func (c *KMSCipher) DecryptCustomer(ctx context.Context, scope CustomerCredentialScope, ciphertext []byte, storedKeyARN string) ([]byte, error) {
	if storedKeyARN != c.keyARN {
		return nil, fmt.Errorf("row names KMS key %q, expected %q", storedKeyARN, c.keyARN)
	}
	output, err := c.client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:      ciphertext,
		KeyId:               &c.keyARN,
		EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault,
		EncryptionContext:   customerEncryptionContext(scope),
	})
	if err != nil {
		if output != nil {
			clear(output.Plaintext)
		}
		return nil, err
	}
	if output == nil {
		return nil, errors.New("KMS returned no customer decryption result")
	}
	if output.KeyId == nil || *output.KeyId != c.keyARN {
		clear(output.Plaintext)
		return nil, errors.New("KMS decrypted with an unexpected key")
	}
	return output.Plaintext, nil
}

func encryptionContext(scope Scope) map[string]string {
	return map[string]string{
		contextProvider: string(scope.Provider),
		contextKeyID:    scope.KeyID,
	}
}

func customerEncryptionContext(scope CustomerCredentialScope) map[string]string {
	return map[string]string{
		contextCredentialClass:    customerCredentialClassBYOK,
		contextProvider:           string(scope.Provider),
		contextOwnerAccountID:     scope.OwnerAccountID,
		contextConnectionID:       scope.ConnectionID,
		contextEnvironment:        string(scope.Environment),
		contextCredentialHandle:   scope.CredentialHandle,
		contextCredentialRevision: fmt.Sprintf("%d", scope.Revision),
	}
}
