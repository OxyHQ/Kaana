package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/openrouterkey"
)

const inspectOpenRouterKeyUsage = "usage: kaana-credentials inspect-openrouter-key --key-id <exact-id> --expected-organization-id <id> [--expected-workspace-id <id>]"

// exactCredentialCustody lends one exact decrypted credential to a callback.
type exactCredentialCustody interface {
	WithCredential(context.Context, credentialstore.Scope, func([]byte) error) error
}

type openRouterKeyInspector interface {
	Inspect(context.Context, []byte, openrouterkey.Expectation) (openrouterkey.Report, error)
}

// parseInspectOpenRouterKey accepts no provider and no endpoint: the provider
// is always openrouter and the URL is openrouterkey.Endpoint.
func parseInspectOpenRouterKey(arguments []string) (credentialstore.Scope, openrouterkey.Expectation, error) {
	flags := flag.NewFlagSet("inspect-openrouter-key", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	keyID := flags.String("key-id", "", "exact opaque key id")
	organizationID := flags.String("expected-organization-id", "", "expected OpenRouter organization id")
	workspaceID := flags.String("expected-workspace-id", "", "optional expected OpenRouter workspace id")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *keyID == "" {
		return credentialstore.Scope{}, openrouterkey.Expectation{}, errors.New(inspectOpenRouterKeyUsage)
	}
	expected := openrouterkey.Expectation{OrganizationID: *organizationID, WorkspaceID: *workspaceID}
	if err := expected.Validate(); err != nil {
		return credentialstore.Scope{}, openrouterkey.Expectation{}, err
	}
	return credentialstore.Scope{Provider: contract.ProviderSlug("openrouter"), KeyID: *keyID}, expected, nil
}

// inspectOpenRouterKey decrypts the exact key, makes the one GET, writes the
// allow-listed report and fails closed unless ownership positively matched.
func inspectOpenRouterKey(ctx context.Context, custody exactCredentialCustody, inspector openRouterKeyInspector, scope credentialstore.Scope, expected openrouterkey.Expectation, stdout io.Writer) error {
	var report openrouterkey.Report
	var inspectionErr error
	inspected := false
	custodyErr := custody.WithCredential(ctx, scope, func(secret []byte) error {
		inspected = true
		report, inspectionErr = inspector.Inspect(ctx, secret, expected)
		return inspectionErr
	})
	if inspectionErr != nil {
		return safeInspectionError(inspectionErr)
	}
	if custodyErr != nil {
		if errors.Is(custodyErr, credentialstore.ErrCredentialNotActive) {
			return credentialstore.ErrCredentialNotActive
		}
		return credentialstore.ErrCredentialUnavailable
	}
	if !inspected {
		return credentialstore.ErrCredentialUnavailable
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil || len(encoded) > 4096 {
		return errInspectionOutput
	}
	encoded = append(encoded, '\n')
	if n, err := stdout.Write(encoded); err != nil || n != len(encoded) {
		return errInspectionOutput
	}
	return report.Verdict()
}

var errInspectionOutput = errors.New("openrouter key inspection: output failed")

// Return only a trusted sentinel, never the underlying error or its wrapping
// chain: even an error wrapping a safe category may also carry a credential.
func safeInspectionError(err error) error {
	for _, safe := range []error{
		openrouterkey.ErrInvalidExpectation, openrouterkey.ErrInvalidCredential,
		openrouterkey.ErrRequestFailed, openrouterkey.ErrTimeout,
		openrouterkey.ErrRedirectRefused, openrouterkey.ErrCredentialRejected,
		openrouterkey.ErrUnexpectedStatus, openrouterkey.ErrResponseTooLarge,
		openrouterkey.ErrMalformedResponse, openrouterkey.ErrExpectationNotMet,
	} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return openrouterkey.ErrRequestFailed
}
