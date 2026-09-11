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
	parameters := map[string]Scope{
		"/oxy/alia/PROVIDER_KEY_ELEVENLABS":            {Provider: "elevenlabs", KeyID: "legacy-alia-20260901"},
		"/oxy/alia/PROVIDER_KEY_GROQ":                  {Provider: "groq", KeyID: "legacy-alia-20260901"},
		"/oxy/alia/PROVIDER_KEY_OPENROUTER":            {Provider: "openrouter", KeyID: "legacy-alia-20260901"},
		"/oxy/alia/PROVIDER_KEY_XAI":                   {Provider: "xai", KeyID: "legacy-alia-20260901"},
		"/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY":   {Provider: "cerebras", KeyID: "cerebras-relay-main"},
		"/oxy/relay/RELAY_PROVIDER_GROQ_API_KEY":       {Provider: "groq", KeyID: "relay-groq-20260902"},
		"/oxy/relay/RELAY_PROVIDER_OPENROUTER_API_KEY": {Provider: "openrouter", KeyID: "relay-openrouter-20260902"},
		"/oxy/relay/RELAY_PROVIDER_XAI_API_KEY":        {Provider: "xai", KeyID: "relay-xai-20260902"},
	}
	if len(legacyProviderParameters) != 8 || len(parameters) != 8 {
		t.Fatalf("legacy provider handoff count = %d/%d, want 8/8", len(legacyProviderParameters), len(parameters))
	}
	for parameter, scope := range parameters {
		t.Run(parameter, func(t *testing.T) {
			client := &fakeSSMClient{value: "provider-secret"}
			source, err := NewSSMSource(client)
			if err != nil {
				t.Fatalf("NewSSMSource: %v", err)
			}
			secret, err := source.ReadSecureString(context.Background(), parameter, scope)
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

func TestTemporarySSMHandoffsAreExactAndKeepBothCohereIdentities(t *testing.T) {
	parameters := map[string]Scope{
		"/oxy/kaana/provider-key-handoff/20260911/cheaperinference": {Provider: "cheaperinference", KeyID: "e97a886e-ab58-4492-b250-84a944e44276"},
		"/oxy/kaana/provider-key-handoff/20260911/mistral":          {Provider: "mistral", KeyID: "fcb72e20-6b68-418f-bf34-50b58e59744e"},
		"/oxy/kaana/provider-key-handoff/20260911/cohere":           {Provider: "cohere", KeyID: "3574baf0-c7b8-4985-bc5f-94d29b72eafb"},
		"/oxy/kaana/provider-key-handoff/20260911/cohere-2":         {Provider: "cohere", KeyID: "5db11d45-b08a-4b2a-a318-3f88f5d8466a"},
	}
	if len(temporaryProviderCredentialHandoffs) != 4 {
		t.Fatalf("temporary handoff count = %d, want 4", len(temporaryProviderCredentialHandoffs))
	}
	for parameter, scope := range parameters {
		if actual, ok := reviewedProviderCredentialHandoff(parameter); !ok || actual != scope {
			t.Errorf("handoff %q = %+v/%v, want %+v/true", parameter, actual, ok, scope)
		}
		if !validOpaqueCredentialID(scope.KeyID) {
			t.Errorf("handoff %q key ID is not an exact lowercase UUIDv4", parameter)
		}
	}
	if parameters["/oxy/kaana/provider-key-handoff/20260911/cohere"].KeyID == parameters["/oxy/kaana/provider-key-handoff/20260911/cohere-2"].KeyID {
		t.Fatal("the two Cohere credentials share an identity")
	}
	if _, ok := reviewedProviderCredentialHandoff("/oxy/kaana/provider-key-handoff/20260911/cohere/extra"); ok {
		t.Fatal("temporary handoff prefix expansion was accepted")
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
		"/oxy/relay/RELAY_PROVIDER_ANTHROPIC_API_KEY",
		"/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY/extra",
	} {
		client := &fakeSSMClient{value: "provider-secret"}
		source, err := NewSSMSource(client)
		if err != nil {
			t.Fatalf("NewSSMSource: %v", err)
		}
		if _, err := source.ReadSecureString(context.Background(), name, Scope{Provider: "openrouter", KeyID: "legacy-alia-20260901"}); err == nil {
			t.Fatalf("unsafe name %q was accepted", name)
		}
		if client.input != nil {
			t.Fatalf("unsafe name %q reached SSM", name)
		}
	}

	for parameter, scope := range map[string]Scope{
		"/oxy/alia/PROVIDER_KEY_ELEVENLABS":            {Provider: "openrouter", KeyID: "legacy-alia-20260901"},
		"/oxy/alia/PROVIDER_KEY_GROQ":                  {Provider: "xai", KeyID: "legacy-alia-20260901"},
		"/oxy/alia/PROVIDER_KEY_OPENROUTER":            {Provider: "groq", KeyID: "legacy-alia-20260901"},
		"/oxy/alia/PROVIDER_KEY_XAI":                   {Provider: "elevenlabs", KeyID: "legacy-alia-20260901"},
		"/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY":   {Provider: "groq", KeyID: "cerebras-relay-main"},
		"/oxy/relay/RELAY_PROVIDER_GROQ_API_KEY":       {Provider: "openrouter", KeyID: "relay-groq-20260902"},
		"/oxy/relay/RELAY_PROVIDER_OPENROUTER_API_KEY": {Provider: "xai", KeyID: "relay-openrouter-20260902"},
		"/oxy/relay/RELAY_PROVIDER_XAI_API_KEY":        {Provider: "cerebras", KeyID: "relay-xai-20260902"},
	} {
		client := &fakeSSMClient{value: "provider-secret"}
		source, err := NewSSMSource(client)
		if err != nil {
			t.Fatalf("NewSSMSource: %v", err)
		}
		if _, err := source.ReadSecureString(context.Background(), parameter, scope); err == nil {
			t.Fatalf("handoff %q was accepted for scope %+v", parameter, scope)
		}
		if client.input != nil {
			t.Fatalf("mismatched handoff %q reached SSM", parameter)
		}
	}

	source, err := NewSSMSource(&fakeSSMClient{err: errors.New("access denied")})
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	_, err = source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER", Scope{Provider: "openrouter", KeyID: "legacy-alia-20260901"})
	if err == nil || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestSSMImportRefusesPlainStringParameters(t *testing.T) {
	source, err := NewSSMSource(&fakeSSMClient{value: "provider-secret", type_: types.ParameterTypeString})
	if err != nil {
		t.Fatalf("NewSSMSource: %v", err)
	}
	if _, err := source.ReadSecureString(context.Background(), "/oxy/alia/PROVIDER_KEY_OPENROUTER", Scope{Provider: "openrouter", KeyID: "legacy-alia-20260901"}); err == nil {
		t.Fatal("a plaintext String parameter was accepted")
	}
}
