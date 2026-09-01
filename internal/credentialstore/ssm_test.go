package credentialstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type fakeSSMClient struct {
	input *ssm.GetParameterInput
	value string
	err   error
	type_ types.ParameterType
}

func (f *fakeSSMClient) GetParameter(_ context.Context, input *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.input = input
	if f.err != nil {
		return nil, f.err
	}
	parameterType := f.type_
	if parameterType == "" {
		parameterType = types.ParameterTypeSecureString
	}
	return &ssm.GetParameterOutput{Parameter: &types.Parameter{Value: &f.value, Type: parameterType}}, nil
}

func TestSSMImportRequestsDecryptionAndReturnsNoMetadata(t *testing.T) {
	client := &fakeSSMClient{value: "provider-secret"}
	source, err := NewSSMSource(client)
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	secret, err := source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER")
	if err != nil {
		t.Fatalf("ReadSecureString: %v", err)
	}
	defer clear(secret)
	if string(secret) != "provider-secret" {
		t.Fatal("source did not return the decrypted value")
	}
	if client.input == nil || client.input.WithDecryption == nil || !*client.input.WithDecryption {
		t.Fatal("SSM value was requested without decryption")
	}
}

func TestSSMImportRefusesUnsafeInputsWithoutEchoingSecrets(t *testing.T) {
	for _, name := range []string{"", "relative", " /oxy/key", "/legacy/provider-key", "/oxy/alia/DATABASE_URL"} {
		source, err := NewSSMSource(&fakeSSMClient{value: "provider-secret"})
		if err != nil {
			t.Fatalf("NewSSMSource: %v", err)
		}
		if _, err := source.ReadSecureString(context.Background(), name); err == nil {
			t.Fatalf("unsafe name %q was accepted", name)
		}
	}

	source, err := NewSSMSource(&fakeSSMClient{err: errors.New("access denied")})
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	_, err = source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER")
	if err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestSSMImportRefusesPlainStringParameters(t *testing.T) {
	source, err := NewSSMSource(&fakeSSMClient{value: "provider-secret", type_: types.ParameterTypeString})
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	if _, err := source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER"); err == nil {
		t.Fatal("a plaintext String parameter was accepted")
	}
}
