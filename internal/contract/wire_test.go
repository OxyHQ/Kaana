package contract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A required array leaves this process as `[]`, never as `null`.
//
// Go's zero slice is nil and `encoding/json` renders nil as `null`, so a field
// the published contract declares `z.array(...)` — neither nullable nor
// optional — reaches the wire as a value that schema has no spelling for. The
// descriptor gate cannot see it: the Go type IS a slice and the contract DOES
// declare an array, so the shapes agree and only the encoding disagrees.
//
// It is invisible on every happy path, because these fields are empty only when
// nothing was measured — which for a usage report means a generation that
// failed. What it costs is not a log line. Oxy's edge reads the terminal error
// frame, then parses this report; the parse throws, the throw discards the
// failure it had already read, and a provider's refusal reaches the customer as
// an internal error instead. Measured against a provider account with no
// balance, that was every request.
//
// Each case is a shape whose array is legitimately empty for a reason that
// really occurs, paired with the exact bytes the contract requires.
func TestRequiredArraysEncodeAsEmptyNotNull(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		require string
		refuse  string
	}{
		{
			// The production shape: the provider refused the request before it
			// measured anything, so `Units` is never assigned.
			name:    "a failed generation measured no units",
			value:   failedReportWithNothingMeasured(),
			require: `"units":[]`,
			refuse:  `"units":null`,
		},
		{
			// Not reachable through the executor today — an envelope without
			// inference:invoke is refused before a report exists — but it is
			// the same field on the same emitted report, one guard away from
			// live. Fixing only the sibling that happens to be reachable is how
			// the identical failure lands again.
			name:    "a principal carrying no inference scopes",
			value:   AuthenticatedPrincipal{Billing: BillingPrincipal{AccountID: "acc_01JQZ"}},
			require: `"inferenceScopes":[]`,
			refuse:  `"inferenceScopes":null`,
		},
		{
			// The nested position is the one that actually ships: the report
			// carries the principal, and a report-level fix that did not reach
			// inside it would leave the null exactly where Oxy parses it.
			name:    "a report carrying a principal with no scopes",
			value:   reportWithUnscopedPrincipal(),
			require: `"inferenceScopes":[]`,
			refuse:  `"inferenceScopes":null`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(testCase.value)
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			if strings.Contains(string(encoded), testCase.refuse) {
				t.Errorf("the wire bytes carry %s, which the published schema cannot parse:\n%s",
					testCase.refuse, encoded)
			}
			if !strings.Contains(string(encoded), testCase.require) {
				t.Errorf("the wire bytes do not carry %s:\n%s", testCase.require, encoded)
			}
		})
	}
}

// TestAPopulatedReportIsEncodedWhole is the positive control on the encoder
// above.
//
// A MarshalJSON that emptied every array, dropped a field or renamed one would
// satisfy the null check and destroy the report. So a report with real content
// has to survive the round trip byte-identical in meaning: same units, same
// scopes, every field back where it was.
func TestAPopulatedReportIsEncodedWhole(t *testing.T) {
	report := failedReportWithNothingMeasured()
	report.Outcome = OutcomeCompleted
	report.Units = []UsageQuantity{{Unit: UnitInputTokens, Quantity: 314}, {Unit: UnitOutputTokens, Quantity: 204}}
	report.UsageSource = UsageProviderReported
	report.TimeToFirstTokenMs = pointerTo(180)

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if !strings.Contains(string(encoded), `"units":[{"unit":"input_tokens","quantity":314}`) {
		t.Errorf("the measured units did not survive the encoding:\n%s", encoded)
	}
	if !strings.Contains(string(encoded), `"inferenceScopes":["inference:invoke","inference:models:read"]`) {
		t.Errorf("the principal's scopes did not survive the encoding:\n%s", encoded)
	}

	var decoded UsageReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the encoded report does not decode: %v", err)
	}
	if !reflect.DeepEqual(report, decoded) {
		t.Errorf("the report did not round-trip:\n sent %+v\n back %+v", report, decoded)
	}
}

func TestCompletedReportCannotClaimNoMeasuredUsage(t *testing.T) {
	report := failedReportWithNothingMeasured()
	report.Outcome = OutcomeCompleted
	if err := report.Validate(); err == nil || !strings.Contains(err.Error(), "at least one usage unit") {
		t.Fatalf("completed empty report validation = %v", err)
	}

	report.Units = []UsageQuantity{{Unit: UnitRequests, Quantity: 1}}
	if err := report.Validate(); err != nil {
		t.Fatalf("completed measured report validation = %v", err)
	}
}

// failedReportWithNothingMeasured is the report the executor builds when a
// provider refuses before producing anything — a 402 from an account with no
// balance, measured in production. `Units` is deliberately left at its zero
// value: assigning an empty slice here would test the fixture, not the encoder.
func failedReportWithNothingMeasured() UsageReport {
	return UsageReport{
		SchemaVersion:          UsageReportSchemaVersion,
		RequestID:              "req_01JQZABCDEF",
		GenerationID:           pointerTo(GenerationID("gen_01JQZABCDEF")),
		Attribution:            sampleAttribution(),
		Outcome:                OutcomeFailed,
		UsageSource:            UsageEstimated,
		ResolvedModelReference: "openai/gpt-5@2026-05-01",
		ServingProvider:        "openai",
		DeploymentID:           DeploymentID("dep_openai_gpt5_use1"),
		RouteSwitches:          0,
		StartedAt:              "2026-08-16T09:41:00.000Z",
		CompletedAt:            "2026-08-16T09:41:02.500Z",
	}
}

func reportWithUnscopedPrincipal() UsageReport {
	report := failedReportWithNothingMeasured()
	report.Attribution.Principal.InferenceScopes = nil
	return report
}
