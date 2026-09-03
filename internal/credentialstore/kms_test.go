package credentialstore

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

const kmsTestKeyARN = "arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000001"

type fakeKMSClient struct {
	encryptInput     *kms.EncryptInput
	decryptInput     *kms.DecryptInput
	decryptKeyARN    string
	decryptPlaintext []byte
}

func (f *fakeKMSClient) Encrypt(_ context.Context, input *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	f.encryptInput = input
	keyARN := kmsTestKeyARN
	return &kms.EncryptOutput{CiphertextBlob: []byte("ciphertext"), KeyId: &keyARN}, nil
}

func (f *fakeKMSClient) Decrypt(_ context.Context, input *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	f.decryptInput = input
	keyARN := kmsTestKeyARN
	if f.decryptKeyARN != "" {
		keyARN = f.decryptKeyARN
	}
	plaintext := f.decryptPlaintext
	if plaintext == nil {
		plaintext = []byte("plaintext")
	}
	return &kms.DecryptOutput{Plaintext: plaintext, KeyId: &keyARN}, nil
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

func TestKMSKeyIdentityIsNeverWhitespaceNormalized(t *testing.T) {
	for _, keyARN := range []string{" " + kmsTestKeyARN, kmsTestKeyARN + " "} {
		if _, err := NewKMSCipher(&fakeKMSClient{}, keyARN); err == nil {
			t.Fatalf("KMS key ARN %q was normalized instead of refused", keyARN)
		}
	}
}

func TestCustomerKMSContextBindsEveryExactIdentityMember(t *testing.T) {
	client := &fakeKMSClient{}
	cipher, err := NewKMSCipher(client, kmsTestKeyARN)
	if err != nil {
		t.Fatalf("NewKMSCipher: %v", err)
	}
	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: CustomerCredentialIdentity{
			Provider:       "anthropic",
			OwnerAccountID: "acc_customer_01",
			ConnectionID:   "conn_customer_01",
			Environment:    contract.EnvironmentProduction,
		},
		CredentialHandle: fixedCustomerHandle,
		Revision:         7,
	}
	ciphertext, keyARN, err := cipher.EncryptCustomer(context.Background(), scope, []byte("plaintext"))
	if err != nil {
		t.Fatalf("EncryptCustomer: %v", err)
	}
	expected := map[string]string{
		contextCredentialClass:    customerCredentialClassBYOK,
		contextProvider:           "anthropic",
		contextOwnerAccountID:     "acc_customer_01",
		contextConnectionID:       "conn_customer_01",
		contextEnvironment:        "production",
		contextCredentialHandle:   fixedCustomerHandle,
		contextCredentialRevision: "7",
	}
	if !reflect.DeepEqual(client.encryptInput.EncryptionContext, expected) {
		t.Fatalf("encrypt context = %#v, expected %#v", client.encryptInput.EncryptionContext, expected)
	}
	if _, err := cipher.DecryptCustomer(context.Background(), scope, ciphertext, keyARN); err != nil {
		t.Fatalf("DecryptCustomer: %v", err)
	}
	if !reflect.DeepEqual(client.decryptInput.EncryptionContext, expected) {
		t.Fatalf("decrypt context = %#v, expected %#v", client.decryptInput.EncryptionContext, expected)
	}
}

func TestKMSClearsCustomerPlaintextReturnedUnderAnUnexpectedKey(t *testing.T) {
	plaintext := []byte("customer-secret")
	client := &fakeKMSClient{
		decryptKeyARN:    "arn:aws:kms:us-west-2:123456789012:key/unexpected",
		decryptPlaintext: plaintext,
	}
	cipher, err := NewKMSCipher(client, kmsTestKeyARN)
	if err != nil {
		t.Fatalf("NewKMSCipher: %v", err)
	}
	scope := CustomerCredentialScope{
		CustomerCredentialIdentity: customerTestIdentity,
		CredentialHandle:           fixedCustomerHandle,
		Revision:                   1,
	}
	if _, err := cipher.DecryptCustomer(context.Background(), scope, []byte("ciphertext"), kmsTestKeyARN); err == nil {
		t.Fatal("KMS plaintext returned under another key was accepted")
	}
	if !bytes.Equal(plaintext, make([]byte, len(plaintext))) {
		t.Fatal("KMS plaintext survived the unexpected-key error path")
	}
}
