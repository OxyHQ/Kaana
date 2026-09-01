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
const legacyProviderParameterPrefix = "/oxy/alia/PROVIDER_KEY_"

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
func (s *SSMSource) ReadSecureString(ctx context.Context, parameterName string) ([]byte, error) {
	name := strings.TrimSpace(parameterName)
	if name == "" || name != parameterName || !strings.HasPrefix(name, legacyProviderParameterPrefix) || len(name) > 2048 {
		return nil, fmt.Errorf("credential import: parameter must be one legacy %s* handoff path", legacyProviderParameterPrefix)
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
