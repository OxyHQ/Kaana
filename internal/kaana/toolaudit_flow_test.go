package kaana_test

import (
	"context"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/deploymentstats"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// toolCallingAdapter serves a stream whose only output is the given tool calls.
func toolCallingAdapter(calls ...provider.ToolCallDelta) *scriptedAdapter {
	return &scriptedAdapter{slug: "stub", stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		for _, toolCall := range calls {
			if err := out.ToolCall(toolCall); err != nil {
				return provider.Outcome{}, err
			}
		}
		units := []contract.UsageQuantity{
			{Unit: contract.UnitRequests, Quantity: 1},
			{Unit: contract.UnitOutputTokens, Quantity: 8},
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishToolCalls}, nil
	}}
}

func toolRequest() *contract.Request {
	request := baseRequest()
	request.Tools = []contract.ToolDefinition{{Type: "function", Name: "lookup", Parameters: map[string]any{"type": "object"}}}
	return request
}

func fiveMinutes(t *testing.T, stats *deploymentstats.Registry) deploymentstats.Window {
	t.Helper()
	snapshot := stats.Project([]contract.DeploymentID{"dep_test"})
	if len(snapshot) != 1 {
		t.Fatalf("projected %d deployments", len(snapshot))
	}
	return snapshot[0].Windows[0]
}

// TestToolCallOutputReachesTheDeploymentsRecentBehaviour: the executor must
// feed what the emitter judged into the per-deployment record, and judge each
// attempt on its own calls.
func TestToolCallOutputReachesTheDeploymentsRecentBehaviour(t *testing.T) {
	t.Run("well formed calls are a tool-call attempt without an error", func(t *testing.T) {
		stats := deploymentstats.NewRegistry(nil)
		adapter := toolCallingAdapter(
			provider.ToolCallDelta{ID: "c1", Name: "lookup", ArgumentsDelta: `{"q":"kaana"}`, Complete: true},
		)
		_, result := harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, stats: stats}.run(t, toolRequest())
		if result.Failure != nil {
			t.Fatalf("the request failed: %v", result.Failure)
		}
		if w := fiveMinutes(t, stats); w.ToolCallAttempts != 1 || w.ToolCallErrors != 0 {
			t.Fatalf("tool call attempts/errors = %d/%d, want 1/0", w.ToolCallAttempts, w.ToolCallErrors)
		}
	})
	t.Run("a call to an undeclared tool is counted as an error", func(t *testing.T) {
		stats := deploymentstats.NewRegistry(nil)
		adapter := toolCallingAdapter(
			provider.ToolCallDelta{ID: "c1", Name: "lookup", ArgumentsDelta: `{}`, Complete: true},
			provider.ToolCallDelta{ID: "c2", Name: "rm_rf", ArgumentsDelta: `{}`, Complete: true},
		)
		harness{deployments: oneDeployment, adapters: []provider.Adapter{adapter}, stats: stats}.run(t, toolRequest())
		if w := fiveMinutes(t, stats); w.ToolCallAttempts != 1 || w.ToolCallErrors != 1 {
			t.Fatalf("tool call attempts/errors = %d/%d, want 1/1 (one attempt, however many calls)", w.ToolCallAttempts, w.ToolCallErrors)
		}
	})
	t.Run("control: text-only output is not in the tool-call denominator", func(t *testing.T) {
		stats := deploymentstats.NewRegistry(nil)
		harness{deployments: oneDeployment, adapters: []provider.Adapter{succeedingAdapter("stub", 5)}, stats: stats}.run(t, toolRequest())
		w := fiveMinutes(t, stats)
		if w.Attempts != 1 || w.ToolCallAttempts != 0 {
			t.Fatalf("attempts/tool call attempts = %d/%d, want 1/0", w.Attempts, w.ToolCallAttempts)
		}
	})
}
