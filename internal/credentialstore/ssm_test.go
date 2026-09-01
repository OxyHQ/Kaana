package credentialstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
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
	for _, parameter := range []string{
		"/oxy/alia/PROVIDER_KEY_OPENROUTER",
		"/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY",
	} {
		t.Run(parameter, func(t *testing.T) {
			client := &fakeSSMClient{value: "provider-secret"}
			source, err := NewSSMSource(client)
			if err != nil {
				t.Fatalf("NewSSMSource: %v", err)
			}
			provider := contract.ProviderSlug("openrouter")
			if parameter == "/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY" {
				provider = "cerebras"
			}
			secret, err := source.ReadSecureString(context.Background(), parameter, provider)
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
			if client.input.Name == nil || *client.input.Name != parameter {
				t.Fatalf("SSM request name = %v, want the exact reviewed path", client.input.Name)
			}
		})
	}
}

func TestSSMImportRefusesUnsafeInputsWithoutEchoingSecrets(t *testing.T) {
	for _, name := range []string{
		"",
		"relative",
		" /oxy/key",
		"/legacy/provider-key",
		"/oxy/alia/DATABASE_URL",
		"/oxy/alia/PROVIDER_KEY_",
		"/oxy/relay/RELAY_PROVIDER_GROQ_API_KEY",
		"/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY/extra",
	} {
		client := &fakeSSMClient{value: "provider-secret"}
		source, err := NewSSMSource(client)
		if err != nil {
			t.Fatalf("NewSSMSource: %v", err)
		}
		if _, err := source.ReadSecureString(context.Background(), name, "openrouter"); err == nil {
			t.Fatalf("unsafe name %q was accepted", name)
		}
		if client.input != nil {
			t.Fatalf("unsafe name %q reached SSM", name)
		}
	}

	client := &fakeSSMClient{value: "provider-secret"}
	source, err := NewSSMSource(client)
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	if _, err := source.ReadSecureString(context.Background(), "/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY", "openrouter"); err == nil {
		t.Fatal("the exact Relay Cerebras handoff was accepted for a different provider identity")
	}
	if client.input != nil {
		t.Fatal("a mismatched provider identity reached SSM")
	}

	source, err = NewSSMSource(&fakeSSMClient{err: errors.New("access denied")})
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	_, err = source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER", "openrouter")
	if err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestSSMImportRefusesPlainStringParameters(t *testing.T) {
	source, err := NewSSMSource(&fakeSSMClient{value: "provider-secret", type_: types.ParameterTypeString})
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	if _, err := source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER", "openrouter"); err == nil {
		t.Fatal("a plaintext String parameter was accepted")
	}
}
