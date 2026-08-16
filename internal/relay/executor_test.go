package relay_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/relay"
)

const testInventory = `{"deployments":[{
  "deploymentId":"dep_test","provider":"stub",
  "modelReference":"stub/model@2026-05-01","upstreamModelId":"model",
  "region":"test-region","current":true}]}`

/* -------------------------------------------------------------------------- */
/*  Adapters the tests script                                                 */
/* -------------------------------------------------------------------------- */

type scriptedAdapter struct {
	stream func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error)
}

func (s *scriptedAdapter) Provider() contract.ProviderSlug { return "stub" }

func (s *scriptedAdapter) Translate(request *contract.Request, route provider.Route) (*provider.Call, error) {
	return &provider.Call{Route: route, Stream: request.Stream}, nil
}

func (s *scriptedAdapter) Stream(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
	return s.stream(ctx, call, out)
}

func (s *scriptedAdapter) Health(context.Context) provider.Health {
	return provider.Health{Provider: "stub", Status: provider.HealthOK, CheckedAt: contract.NewTimestamp(time.Now())}
}

func execute(t *testing.T, adapter provider.Adapter, request *contract.Request) ([]contract.StreamEvent, relay.Result) {
	t.Helper()
	inv, err := inventory.Parse([]byte(testInventory))
	if err != nil {
		t.Fatalf("parsing the inventory: %v", err)
	}
	registry, err := provider.NewRegistry(adapter)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	var events []contract.StreamEvent
	result := relay.NewExecutor(inv, registry).Execute(context.Background(), request, func(event contract.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	return events, result
}

func baseRequest() *contract.Request {
	reference := contract.ModelReference("stub/model@2026-05-01")
	text := "hi"
	return &contract.Request{
		SchemaVersion: contract.RequestEnvelopeVersion,
		Attribution: contract.Attribution{
			Principal: contract.AuthenticatedPrincipal{
				Billing:         contract.BillingPrincipal{AccountID: "acc_test"},
				ApplicationID:   "app_test",
				CredentialID:    "cred_test",
				Environment:     contract.EnvironmentDevelopment,
				InferenceScopes: []contract.Scope{contract.ScopeInvoke},
			},
			RequestID: "req_test",
		},
		Target:   contract.RoutingTarget{Kind: contract.TargetModel, ModelReference: &reference},
		Modality: contract.ModalityText,
		Input: contract.Input{
			Format: contract.InputMessages,
			Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &text}},
			}},
		},
		Stream: true,
		Client: contract.ClientRequestMetadata{
			APIFormat:  contract.APIFormatResponses,
			Endpoint:   "/v1/responses",
			ReceivedAt: contract.NewTimestamp(time.Now()),
		},
		RoutingPolicy: contract.RoutingPolicyReference{RoutingPolicyID: "rp", PolicyVersion: 1},
	}
}

func happyAdapter() *scriptedAdapter {
	return &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "hello"); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}}
		if err := out.Usage(units, contract.UsageProviderReported); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}
}

/* -------------------------------------------------------------------------- */
/*  Refusals                                                                  */
/* -------------------------------------------------------------------------- */

// TestARoutingProfileTargetIsRefusedWithTheFieldNamed pins the one place this
// build knowingly serves less than the contract describes. Resolving a profile
// needs its candidate list, and the envelope carries a routing policy REFERENCE
// rather than a snapshot — so choosing a model here would be exactly the silent
// substitution the platform forbids.
func TestARoutingProfileTargetIsRefusedWithTheFieldNamed(t *testing.T) {
	request := baseRequest()
	profile := contract.RoutingProfileSlug("auto")
	request.Target = contract.RoutingTarget{Kind: contract.TargetRoutingProfile, RoutingProfile: &profile}

	events, result := execute(t, happyAdapter(), request)

	if result.Failure == nil {
		t.Fatal("a routing-profile target was served")
	}
	if result.Failure.Code != contract.CodeInvalidRequest {
		t.Errorf("refused with %q", result.Failure.Code)
	}
	if result.Failure.Retryable {
		t.Error("the refusal is retryable, but no retry can add a policy snapshot to the envelope")
	}
	if result.Failure.Param == nil || *result.Failure.Param != "target.routingProfile" {
		t.Errorf("the refusal names %v as the field at fault", result.Failure.Param)
	}
	if len(events) != 1 || events[0].EventType() != contract.EventError {
		t.Errorf("the refusal produced %d events", len(events))
	}
}

func TestAnEnvelopeWithoutTheInvokeScopeIsRefused(t *testing.T) {
	request := baseRequest()
	request.Attribution.Principal.InferenceScopes = []contract.Scope{contract.ScopeModelsRead}

	_, result := execute(t, happyAdapter(), request)

	if result.Failure == nil {
		t.Fatal("an envelope without inference:invoke was served")
	}
	if result.Failure.Code != contract.CodeInsufficientScope {
		t.Errorf("refused with %q", result.Failure.Code)
	}
}

func TestAnUnroutableModelIsRefusedAsNotFound(t *testing.T) {
	request := baseRequest()
	reference := contract.ModelReference("stub/other@2026-05-01")
	request.Target.ModelReference = &reference

	_, result := execute(t, happyAdapter(), request)

	if result.Failure == nil {
		t.Fatal("an unroutable model was served")
	}
	if result.Failure.Code != contract.CodeModelNotFound {
		t.Errorf("refused with %q", result.Failure.Code)
	}
	if result.Failure.Retryable {
		t.Error("model_not_found was reported retryable; no identical retry makes a route appear")
	}
}

/* -------------------------------------------------------------------------- */
/*  Framing the emitter enforces                                              */
/* -------------------------------------------------------------------------- */

func TestTheEmitterRefusesAStreamTheContractCannotDescribe(t *testing.T) {
	cases := []struct {
		name   string
		stream func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error)
		expect string
	}{
		{
			name: "output before the stream started",
			stream: func(_ context.Context, _ *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				return provider.Outcome{}, out.Delta(0, contract.ChannelOutputText, "hello")
			},
			expect: "precedes the stream's start event",
		},
		{
			name: "two start events",
			stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
					return provider.Outcome{}, err
				}
				return provider.Outcome{}, out.Start(call.Route.ModelReference, time.Now())
			},
			expect: "second start event",
		},
		{
			name: "a start event naming an unpinned model",
			stream: func(_ context.Context, _ *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				return provider.Outcome{}, out.Start("stub/model", time.Now())
			},
			expect: "not revision-pinned",
		},
		{
			name: "a usage event carrying no units",
			stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
					return provider.Outcome{}, err
				}
				return provider.Outcome{}, out.Usage(nil, contract.UsageProviderReported)
			},
			expect: "at least one unit",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			adapter := &scriptedAdapter{stream: func(ctx context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
				err := func() error {
					_, err := testCase.stream(ctx, call, out)
					return err
				}()
				if err == nil {
					t.Fatal("the emitter accepted an event it should have refused")
				}
				if !strings.Contains(err.Error(), testCase.expect) {
					t.Errorf("the emitter refused with %q, expected it to mention %q", err, testCase.expect)
				}
				// Returned so the executor treats this as a failed request,
				// which is what it is.
				return provider.Outcome{}, err
			}}
			_, result := execute(t, adapter, baseRequest())
			if result.Failure == nil {
				t.Error("a stream the contract cannot describe was reported as a success")
			}
		})
	}
}

// TestAnAdapterThatCompletesWithoutStartingIsAFailure covers the shape that
// would otherwise hand settlement a receipt for output nobody saw.
func TestAnAdapterThatCompletesWithoutStartingIsAFailure(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(context.Context, *provider.Call, provider.Emitter) (provider.Outcome, error) {
		return provider.Outcome{
			Units:       []contract.UsageQuantity{{Unit: contract.UnitOutputTokens, Quantity: 500}},
			UsageSource: contract.UsageProviderReported,
		}, nil
	}}

	_, result := execute(t, adapter, baseRequest())

	if result.Failure == nil {
		t.Fatal("an adapter that never started a stream was reported as a success")
	}
	if result.Report == nil || result.Report.Outcome != contract.OutcomeFailed {
		t.Errorf("the report says %v", result.Report)
	}
}

/* -------------------------------------------------------------------------- */
/*  Settlement                                                                */
/* -------------------------------------------------------------------------- */

func TestASuccessfulRequestProducesASettleableReport(t *testing.T) {
	events, result := execute(t, happyAdapter(), baseRequest())

	if result.Failure != nil {
		t.Fatalf("the request failed: %v", result.Failure)
	}
	if result.Report == nil {
		t.Fatal("no usage report was produced")
	}
	if err := result.Report.Validate(); err != nil {
		t.Fatalf("the report would be rejected by the contract: %v", err)
	}
	if result.Report.Outcome != contract.OutcomeCompleted {
		t.Errorf("the report says %q", result.Report.Outcome)
	}
	if result.Report.GenerationID == nil || *result.Report.GenerationID == "" {
		t.Error("no generation id was allocated, so the request has no receipt handle")
	}
	if last := events[len(events)-1]; last.EventType() != contract.EventDone {
		t.Errorf("the stream ends with %q", last.EventType())
	}
	// The data plane measures; the control plane prices. A receipt id on a
	// done event would be Relay quoting a settlement it did not compute.
	done := events[len(events)-1].(*contract.StreamDoneEvent)
	if done.ReceiptID != nil {
		t.Error("the done event carries a receipt id, which only settlement can produce")
	}
}

// TestAnAdapterThatMeasuresNothingStillSaysHowItKnows: an estimate that is
// indistinguishable from a provider's own count is one nobody can reconcile.
func TestAnAdapterThatMeasuresNothingStillSaysHowItKnows(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{FinishReason: contract.FinishStop}, nil
	}}

	_, result := execute(t, adapter, baseRequest())

	if result.Report == nil {
		t.Fatal("no report was produced")
	}
	if result.Report.UsageSource != contract.UsageEstimated {
		t.Errorf("a report with no measurement claims source %q", result.Report.UsageSource)
	}
}

// TestAReportTheContractWouldRejectIsNotReturnedAsIfItWereFine: an invalid
// usage report is not a lost log line — it is the record settlement runs
// against, so a request that executed and cannot be settled must say so.
func TestAReportTheContractWouldRejectIsNotReturnedAsIfItWereFine(t *testing.T) {
	adapter := &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{
			// One unit reported twice: the contract's usage report refuses it,
			// because a unit is settled once, as a total.
			Units: []contract.UsageQuantity{
				{Unit: contract.UnitOutputTokens, Quantity: 10},
				{Unit: contract.UnitOutputTokens, Quantity: 20},
			},
			UsageSource:  contract.UsageProviderReported,
			FinishReason: contract.FinishStop,
		}, nil
	}}

	_, result := execute(t, adapter, baseRequest())

	if result.Report != nil {
		t.Fatal("a report the contract would reject was returned as if it were settleable")
	}
	if result.Failure == nil || result.Failure.Code != contract.CodeInternalError {
		t.Fatalf("the unsettleable request reported %v", result.Failure)
	}
}
