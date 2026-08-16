package openaicompat

import (
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/provider/conformance"
)

// fakeAPIKey is a test credential and nothing else. The conformance suite
// asserts this exact string never reaches a customer, which is the only reason
// it is a literal rather than a random value: the assertion needs something to
// look for. It deliberately avoids any real provider's key prefix so a secret
// scanner has nothing to flag. No real provider key appears in this repository,
// in a test, or in CI.
const fakeAPIKey = "relay-conformance-fake-credential-0000"

// TestOpenAICompatConformance runs the whole provider suite against the ported
// adapter.
func TestOpenAICompatConformance(t *testing.T) {
	conformance.Run(t, subject("openai"))
}

// TestOneProtocolServesSeveralProviders is the claim the port is built on: the
// six other Alia adapters that speak this protocol differ from `openai` only by
// base URL, so serving one of them must be a Config, not a rewrite.
//
// It is a test rather than a comment because "the abstraction generalises" is
// exactly the kind of claim that stops being true without anybody noticing. If
// a provider-specific behaviour ever leaks into this package, this run fails
// while the `openai` one still passes.
func TestOneProtocolServesSeveralProviders(t *testing.T) {
	for _, slug := range []contract.ProviderSlug{"together", "xai", "cerebras"} {
		t.Run(string(slug), func(t *testing.T) {
			conformance.Run(t, subject(slug))
		})
	}
}

func subject(slug contract.ProviderSlug) conformance.Subject {
	return conformance.Subject{
		Name:            string(slug),
		Provider:        slug,
		ModelReference:  contract.ModelReference(string(slug) + "/test-model@2026-05-01"),
		UpstreamModelID: "test-model-2026-05-01",
		APIKey:          fakeAPIKey,

		NewAdapter: func(t *testing.T, upstreamURL string) provider.Adapter {
			t.Helper()
			adapter, err := New(Config{Provider: slug, BaseURL: upstreamURL, APIKey: fakeAPIKey})
			if err != nil {
				t.Fatalf("building the %s adapter: %v", slug, err)
			}
			return adapter
		},

		NewUnconfigured: func(t *testing.T) provider.Adapter {
			t.Helper()
			adapter, err := New(Config{Provider: slug, BaseURL: "https://unreachable.invalid"})
			if err != nil {
				t.Fatalf("building the unconfigured %s adapter: %v", slug, err)
			}
			return adapter
		},

		StartUpstream: startFakeUpstream,

		// An embedding request is something chat completions genuinely cannot
		// express, as opposed to something this adapter merely has not
		// implemented.
		Unsupported: func() (*contract.Request, contract.ErrorCode) {
			reference := contract.ModelReference(string(slug) + "/test-model@2026-05-01")
			text := "embed me"
			return &contract.Request{
				SchemaVersion: contract.RequestEnvelopeVersion,
				Attribution: contract.Attribution{
					Principal: contract.AuthenticatedPrincipal{
						Billing:         contract.BillingPrincipal{AccountID: "acc_conformance"},
						ApplicationID:   "app_conformance",
						CredentialID:    "cred_conformance",
						Environment:     contract.EnvironmentDevelopment,
						InferenceScopes: []contract.Scope{contract.ScopeInvoke},
					},
					RequestID: "req_conformance_unsupported",
				},
				Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
				Modality: contract.ModalityEmbedding,
				Input:    contract.Input{Format: contract.InputText, Text: &text},
				Stream:   false,
				Client: contract.ClientRequestMetadata{
					APIFormat:  contract.APIFormatEmbeddings,
					Endpoint:   "/v1/embeddings",
					ReceivedAt: contract.NewTimestamp(time.Now()),
				},
				RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp_conformance", PolicyVersion: 1},
			}, contract.CodeUnsupportedModality
		},
	}
}
