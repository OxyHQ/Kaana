package anthropic

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
// scanner has nothing to flag — and, not incidentally, so that the contract's
// credential-SHAPE pattern cannot be what saves it. No real provider key
// appears in this repository, in a test, or in CI.
const (
	fakeAPIKey       = "relay-conformance-fake-credential-0000"
	fakeSecondAPIKey = "relay-conformance-fake-credential-0001"
)

const fakeModelReference = contract.ModelReference("anthropic/claude-fake@2026-05-01")

// TestAnthropicConformance runs the whole provider suite against the adapter.
func TestAnthropicConformance(t *testing.T) {
	conformance.Run(t, subject())
}

func subject() conformance.Subject {
	return conformance.Subject{
		Name:            "anthropic",
		Provider:        Slug,
		ModelReference:  fakeModelReference,
		UpstreamModelID: "claude-fake-2026-05-01",
		APIKeys:         []string{fakeAPIKey, fakeSecondAPIKey},

		// The fake's own numbers, restated as the physical request they
		// describe. `input_tokens` here EXCLUDES both cache counts, so the
		// prompt total is the sum of all three — the opposite of what an
		// OpenAI-compatible provider reports, and the reason the normalising
		// arithmetic is per-adapter while the invariant is not.
		StreamedUsage: conformance.StreamedUsage{
			PromptTokens:       fakeInputTokens + fakeCacheCreationTokens + fakeCacheReadTokens,
			CachedPromptTokens: fakeCacheReadTokens,
			OutputTokens:       fakeOutputTokens,
			ReasoningTokens:    fakeThinkingTokens,
		},

		NewAdapter: func(t *testing.T, upstreamURL string) provider.Adapter {
			t.Helper()
			adapter, err := New(Config{BaseURL: upstreamURL, APIKeys: []string{fakeAPIKey, fakeSecondAPIKey}})
			if err != nil {
				t.Fatalf("building the anthropic adapter: %v", err)
			}
			return adapter
		},

		NewUnconfigured: func(t *testing.T) provider.Adapter {
			t.Helper()
			adapter, err := New(Config{BaseURL: "https://unreachable.invalid"})
			if err != nil {
				t.Fatalf("building the unconfigured anthropic adapter: %v", err)
			}
			return adapter
		},

		StartUpstream: startFakeUpstream,

		Refusals: func() []conformance.Refusal {
			embedding := baseRequest("req_conformance_embedding")
			embedding.Modality = contract.ModalityEmbedding
			text := "embed me"
			embedding.Input = contract.Input{Format: contract.InputText, Text: &text}
			embedding.Client.APIFormat = contract.APIFormatEmbeddings
			embedding.Client.Endpoint = "/v1/embeddings"

			// The second refusal is the one a single-adapter suite could not
			// have: a request the CONTRACT considers complete, which this
			// protocol cannot send because it requires an output ceiling on
			// every call. Serving it would mean choosing the customer's ceiling
			// for them.
			unbounded := baseRequest("req_conformance_unbounded")
			unbounded.MaxOutputTokens = nil

			return []conformance.Refusal{
				{
					Name:    "an embedding request",
					Request: embedding,
					Code:    contract.CodeUnsupportedModality,
					Param:   "modality",
				},
				{
					Name:    "a request with no output token limit",
					Request: unbounded,
					Code:    contract.CodeInvalidRequest,
					Param:   "maxOutputTokens",
				},
			}
		},
	}
}

// baseRequest is a well-formed envelope this adapter can serve, which each
// refusal above then breaks in exactly one way.
func baseRequest(requestID contract.RequestID) *contract.Request {
	reference := fakeModelReference
	limit := 256
	text := "hello"
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
			RequestID: requestID,
		},
		Target:          contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality:        contract.ModalityText,
		MaxOutputTokens: &limit,
		Input: contract.Input{
			Format: contract.InputMessages,
			Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &text}},
			}},
		},
		Stream: false,
		Client: contract.ClientRequestMetadata{
			APIFormat:  contract.APIFormatResponses,
			Endpoint:   "/v1/responses",
			ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp_conformance", PolicyVersion: 1},
	}
}
