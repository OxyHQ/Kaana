package credentialstore

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

const kmsTestKeyARN = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000001"

type fakeKMSClient struct {
	encryptInput *kms.EncryptInput
	decryptInput *kms.DecryptInput
}

func (f *fakeKMSClient) Encrypt(_ context.Context, input *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	f.encryptInput = input
	keyARN := kmsTestKeyARN
	return &kms.EncryptOutput{CiphertextBlob: []byte("ciphertext"), KeyId: &keyARN}, nil
}

func (f *fakeKMSClient) Decrypt(_ context.Context, input *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	f.decryptInput = input
	keyARN := kmsTestKeyARN
	return &kms.DecryptOutput{Plaintext: []byte("plaintext"), KeyId: &keyARN}, nil
}

func TestKMSContextBindsCiphertextToProviderAndKeyID(t *testing.T) {
	client := &fakeKMSClient{}
	cipher, err := NewKMSCipher(client, kmsTestKeyARN)
	if err != nil {
		t.Fatalf("NewKMSCipher: %v", err)
	}
	scope := Scope{Provider: "groq", KeyID: "primary"}
	ciphertext, keyARN, err := cipher.Encrypt(context.Background(), scope, []byte("plaintext"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(ciphertext) == 0 || keyARN != kmsTestKeyARN {
		t.Fatalf("ciphertext metadata = %q, %q", ciphertext, keyARN)
	}
	if got := client.encryptInput.EncryptionContext[contextProvider]; got != "groq" {
		t.Fatalf("provider context = %q", got)
	}
	if got := client.encryptInput.EncryptionContext[contextKeyID]; got != "primary" {
		t.Fatalf("key id context = %q", got)
	}

	if _, err := cipher.Decrypt(context.Background(), scope, ciphertext, kmsTestKeyARN); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if client.decryptInput.EncryptionContext[contextProvider] != "groq" || client.decryptInput.EncryptionContext[contextKeyID] != "primary" {
		t.Fatalf("decrypt context = %v", client.decryptInput.EncryptionContext)
	}
}

func TestKMSRefusesARowFromAnotherConfiguredKey(t *testing.T) {
	client := &fakeKMSClient{}
	cipher, err := NewKMSCipher(client, kmsTestKeyARN)
	if err != nil {
		t.Fatalf("NewKMSCipher: %v", err)
	}
	if _, err := cipher.Decrypt(context.Background(), Scope{Provider: "groq", KeyID: "primary"}, []byte("ciphertext"), "arn:aws:kms:us-west-2:123456789012:key/other"); err == nil {
		t.Fatal("row encrypted under another KMS key was accepted")
	}
	if client.decryptInput != nil {
		t.Fatal("KMS was called before the stored key ARN was refused")
	}
}
