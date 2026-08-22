package openaicompat

import (
	"testing"
	"time"

	"github.com/OxyHQ/Pensara/internal/contract"
	"github.com/OxyHQ/Pensara/internal/provider"
	"github.com/OxyHQ/Pensara/internal/provider/conformance"
)

// The fake credentials are test strings and nothing else. The conformance
// suite asserts these exact strings never reach a customer, which is the only
// reason they are literals rather than random values: the assertion needs
// something to look for. They deliberately avoid any real provider's key prefix
// so a secret scanner has nothing to flag. No real provider key appears in this
// repository, in a test, or in CI.
//
// There are two because a pool of one cannot tell an adapter that rotates on
// exhaustion from one that cannot rotate at all.
const (
	fakeAPIKey       = "relay-conformance-fake-credential-0000"
	fakeSecondAPIKey = "relay-conformance-fake-credential-0001"
)

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
		APIKeys:         []string{fakeAPIKey, fakeSecondAPIKey},

		// The fake's own numbers, restated as the physical request they
		// describe. This protocol NESTS both children: `prompt_tokens` (11)
		// includes its 3 cached tokens and `completion_tokens` (5) includes its
		// 2 reasoning tokens, so both subtractions apply here — where the same
		// declaration for Anthropic needs only one of them.
		StreamedUsage: conformance.StreamedUsage{
			PromptTokens:       11,
			CachedPromptTokens: 3,
			OutputTokens:       5,
			ReasoningTokens:    2,
		},

		NewAdapter: func(t *testing.T, upstreamURL string) provider.Adapter {
			t.Helper()
			adapter, err := New(Config{Provider: slug, BaseURL: upstreamURL, APIKeys: []string{fakeAPIKey, fakeSecondAPIKey}})
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
		Refusals: func() []conformance.Refusal {
			reference := contract.ModelReference(string(slug) + "/test-model@2026-05-01")
			text := "embed me"
			embedding := &contract.Request{
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
			}
			return []conformance.Refusal{{
				Name:    "an embedding request",
				Request: embedding,
				Code:    contract.CodeUnsupportedModality,
				Param:   "modality",
			}}
		},
	}
}
