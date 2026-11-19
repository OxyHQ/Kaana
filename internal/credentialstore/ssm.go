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
// 2026-11-19 operator handoff (docs/provider-credential-handoff-20261119.md).
// It is deliberately separate from the frozen legacy allow-list: deleting
// these entries after verification removes the capability without broadening
// historical migration authority.
var temporaryProviderCredentialHandoffs = map[string]Scope{
	"/oxy/kaana/provider-key-handoff/20261119/anthropic": {Provider: "anthropic", KeyID: "7e9c2e7e-fffb-4389-bec6-59647882ff2c"},
}

// temporaryCredentialMetadataHandoffs carries the protected metadata document
// of the same handoff. The document names its funding account's label, which
// belongs neither in this public repository nor in argv, so it crosses the
// same reviewed SecureString boundary as the key.
var temporaryCredentialMetadataHandoffs = map[string]Scope{
	"/oxy/kaana/provider-key-handoff/20261119/anthropic-metadata": {Provider: "anthropic", KeyID: "7e9c2e7e-fffb-4389-bec6-59647882ff2c"},
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
	expectedScope, allowed := reviewedProviderCredentialHandoff(parameterName)
	return s.readReviewed(ctx, parameterName, scope, expectedScope, allowed, "provider-key")
}

// ReadMetadataDocument retrieves the protected metadata document reviewed for
// exactly one handoff identity. The caller still parses and validates it, and
// must refuse a document naming another key.
func (s *SSMSource) ReadMetadataDocument(ctx context.Context, parameterName string, scope Scope) ([]byte, error) {
	expectedScope, allowed := temporaryCredentialMetadataHandoffs[parameterName]
	return s.readReviewed(ctx, parameterName, scope, expectedScope, allowed, "credential-metadata")
}

func (s *SSMSource) readReviewed(ctx context.Context, parameterName string, scope, expectedScope Scope, allowed bool, kind string) ([]byte, error) {
	name := strings.TrimSpace(parameterName)
	if name == "" || name != parameterName || !allowed || len(name) > 2048 {
		return nil, fmt.Errorf("credential import: parameter must be an exact reviewed %s handoff path", kind)
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
