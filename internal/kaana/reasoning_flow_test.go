package kaana_test

import (
	"context"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// reasoningAdapter reasons in one block (its words, then its continuation
// token), and then answers.
func reasoningAdapter() *scriptedAdapter {
	return &scriptedAdapter{slug: "stub", stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelReasoning, "thinking"); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.ReasoningBlock(0, "sealed-token"); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(1, contract.ChannelOutputText, "answer"); err != nil {
			return provider.Outcome{}, err
		}
		units := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitOutputTokens, Quantity: 8}}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}
}

type reasoningSeen struct {
	text   string
	tokens []string
	order  []int
}

func reasoningOf(events []contract.StreamEvent) reasoningSeen {
	var seen reasoningSeen
	for _, event := range events {
		delta, isDelta := event.(*contract.StreamDeltaEvent)
		if !isDelta || delta.Channel != contract.ChannelReasoning {
			continue
		}
		seen.text += delta.Text
		if delta.Opaque != nil {
			seen.tokens = append(seen.tokens, *delta.Opaque)
		}
		seen.order = append(seen.order, event.Sequence())
	}
	return seen
}

func TestAReasoningBlockIsDeliveredOnlyToARequestThatOptedIn(t *testing.T) {
	optedIn := baseRequest()
	optedIn.Reasoning = &contract.ReasoningParameters{}
	events, result := execute(t, reasoningAdapter(), optedIn)
	if result.Failure != nil {
		t.Fatal(result.Failure)
	}
	seen := reasoningOf(events)
	if seen.text != "thinking" || len(seen.tokens) != 1 || seen.tokens[0] != "sealed-token" {
		t.Errorf("an opted-in request saw %+v", seen)
	}
	for index := 1; index < len(events); index++ {
		if events[index].Sequence() != events[index-1].Sequence()+1 {
			t.Errorf("sequence is not contiguous at %d: %d after %d", index, events[index].Sequence(), events[index-1].Sequence())
		}
	}

	// Positive control: without `reasoning` the same model output carries its
	// text, as it always did, and no token: an older reader of the stream never
	// sees the new field.
	events, _ = execute(t, reasoningAdapter(), baseRequest())
	if seen := reasoningOf(events); seen.text != "thinking" || len(seen.tokens) != 0 {
		t.Errorf("a request that never opted in saw %+v", seen)
	}
}

func TestExcludedReasoningIsNeitherStreamedNorTokenised(t *testing.T) {
	request := baseRequest()
	exclude := true
	request.Reasoning = &contract.ReasoningParameters{Exclude: &exclude}
	events, result := execute(t, reasoningAdapter(), request)
	if result.Failure != nil {
		t.Fatal(result.Failure)
	}
	if seen := reasoningOf(events); seen.text != "" || len(seen.tokens) != 0 {
		t.Errorf("excluded reasoning reached the caller: %+v", seen)
	}
	answered := false
	for _, event := range events {
		if delta, isDelta := event.(*contract.StreamDeltaEvent); isDelta && delta.Channel == contract.ChannelOutputText && delta.Text == "answer" {
			answered = true
		}
	}
	if !answered {
		t.Error("excluding reasoning lost the answer")
	}
}
