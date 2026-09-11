package credentialstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const maxImportedCredentialBytes = 4096

var legacyProviderParameters = map[string]Scope{
	"/oxy/alia/PROVIDER_KEY_ELEVENLABS":            {Provider: "elevenlabs", KeyID: "legacy-alia-20260901"},
	"/oxy/alia/PROVIDER_KEY_GROQ":                  {Provider: "groq", KeyID: "legacy-alia-20260901"},
	"/oxy/alia/PROVIDER_KEY_OPENROUTER":            {Provider: "openrouter", KeyID: "legacy-alia-20260901"},
	"/oxy/alia/PROVIDER_KEY_XAI":                   {Provider: "xai", KeyID: "legacy-alia-20260901"},
	"/oxy/relay/RELAY_PROVIDER_CEREBRAS_API_KEY":   {Provider: "cerebras", KeyID: "cerebras-relay-main"},
	"/oxy/relay/RELAY_PROVIDER_GROQ_API_KEY":       {Provider: "groq", KeyID: "relay-groq-20260902"},
	"/oxy/relay/RELAY_PROVIDER_OPENROUTER_API_KEY": {Provider: "openrouter", KeyID: "relay-openrouter-20260902"},
	"/oxy/relay/RELAY_PROVIDER_XAI_API_KEY":        {Provider: "xai", KeyID: "relay-xai-20260902"},
}

// temporaryProviderCredentialHandoffs is a closed, removable bridge for the
// 2026-09-11 operator handoff. It is deliberately separate from the frozen
// legacy allow-list: deleting these entries after verification removes the
// capability without broadening historical migration authority.
var temporaryProviderCredentialHandoffs = map[string]Scope{
	"/oxy/kaana/provider-key-handoff/20260911/cheaperinference": {Provider: "cheaperinference", KeyID: "e97a886e-ab58-4492-b250-84a944e44276"},
	"/oxy/kaana/provider-key-handoff/20260911/mistral":          {Provider: "mistral", KeyID: "fcb72e20-6b68-418f-bf34-50b58e59744e"},
	"/oxy/kaana/provider-key-handoff/20260911/cohere":           {Provider: "cohere", KeyID: "3574baf0-c7b8-4985-bc5f-94d29b72eafb"},
	"/oxy/kaana/provider-key-handoff/20260911/cohere-2":         {Provider: "cohere", KeyID: "5db11d45-b08a-4b2a-a318-3f88f5d8466a"},
}

type ssmClient interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// SSMSource reads a legacy SecureString directly into process memory for a
// one-time migration. It never projects the value into argv, environment, a
// file, or stdout.
type SSMSource struct {
	client ssmClient
}

// OpenSSMSource resolves AWS workload credentials for a one-shot admin task.
func OpenSSMSource(ctx context.Context) (*SSMSource, error) {
	config, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential import: loading AWS configuration: %w", err)
	}
	return NewSSMSource(ssm.NewFromConfig(config))
}

// NewSSMSource builds an SSM migration source from an explicit client.
func NewSSMSource(client ssmClient) (*SSMSource, error) {
	if client == nil {
		return nil, errors.New("credential import: SSM client is required")
	}
	return &SSMSource{client: client}, nil
}

// ReadSecureString retrieves one explicit parameter with KMS decryption. The
// result must be cleared by the caller after it has been re-encrypted.
func (s *SSMSource) ReadSecureString(ctx context.Context, parameterName string, scope Scope) ([]byte, error) {
	name := strings.TrimSpace(parameterName)
	expectedScope, allowed := reviewedProviderCredentialHandoff(name)
	if name == "" || name != parameterName || !allowed || len(name) > 2048 {
		return nil, fmt.Errorf("credential import: parameter must be an exact reviewed provider-key handoff path")
	}
	if scope != expectedScope {
		return nil, errors.New("credential import: the legacy handoff provider/key identity does not match")
	}
	withDecryption := true
	output, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           &name,
		WithDecryption: &withDecryption,
	})
	if err != nil {
		return nil, fmt.Errorf("credential import: reading SSM parameter %q: %w", name, err)
	}
	if output.Parameter == nil || output.Parameter.Value == nil {
		return nil, fmt.Errorf("credential import: SSM parameter %q returned no value", name)
	}
	if output.Parameter.Type != types.ParameterTypeSecureString {
		*output.Parameter.Value = ""
		return nil, fmt.Errorf("credential import: SSM parameter %q is not a SecureString", name)
	}
	secret := []byte(*output.Parameter.Value)
	*output.Parameter.Value = ""
	if len(secret) == 0 {
		return nil, fmt.Errorf("credential import: SSM parameter %q is empty", name)
	}
	if len(secret) > maxImportedCredentialBytes || bytes.ContainsAny(secret, "\r\n") {
		clear(secret)
		return nil, fmt.Errorf("credential import: SSM parameter %q is not one bounded credential", name)
	}
	return secret, nil
}

func legacyProviderCredentialScope(scope Scope) bool {
	for _, expected := range legacyProviderParameters {
		if scope == expected {
			return true
		}
	}
	return false
}

func reviewedProviderCredentialHandoff(name string) (Scope, bool) {
	if scope, ok := temporaryProviderCredentialHandoffs[name]; ok {
		return scope, true
	}
	scope, ok := legacyProviderParameters[name]
	return scope, ok
}

func reviewedProviderCredentialHandoffScope(scope Scope) bool {
	for _, expected := range temporaryProviderCredentialHandoffs {
		if scope == expected {
			return true
		}
	}
	return legacyProviderCredentialScope(scope)
}
