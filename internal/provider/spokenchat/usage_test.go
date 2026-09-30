package spokenchat

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// usageWithCachedAudio reports 100 prompt tokens of which 20 are cached (5 of
// those audio) and 60 are audio, and 250 completion tokens of which 200 are
// audio. The partition is input 25, cached text 15, audio 55, cached audio 5;
// output 50, audio output 200.
const usageWithCachedAudio = `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":250,"total_tokens":350,` +
	`"prompt_tokens_details":{"cached_tokens":20,"audio_tokens":60,"text_tokens":40,"cached_tokens_details":{"audio_tokens":5,"text_tokens":15}},` +
	`"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":200,"text_tokens":50,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}}`

var partitionWithCachedAudio = []contract.UsageQuantity{
	{Unit: contract.UnitRequests, Quantity: 1},
	{Unit: contract.UnitInputTokens, Quantity: 25},
	{Unit: contract.UnitCachedInputTokens, Quantity: 15},
	{Unit: contract.UnitAudioInputTokens, Quantity: 55},
	{Unit: contract.UnitCachedAudioInputTokens, Quantity: 5},
	{Unit: contract.UnitOutputTokens, Quantity: 50},
	{Unit: contract.UnitAudioOutputTokens, Quantity: 200},
}

func TestAudioUsagePartition(t *testing.T) {
	parse := func(raw string) *usage {
		var usage usage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatal(err)
		}
		return &usage
	}
	var wrapped struct {
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(usageWithCachedAudio), &wrapped); err != nil {
		t.Fatal(err)
	}
	units, ok := wrapped.Usage.units()
	if !ok || !reflect.DeepEqual(units, partitionWithCachedAudio) {
		t.Fatalf("units = %+v, %t", units, ok)
	}
	// The partition sums back to the provider's own totals: nothing is billed
	// twice and nothing is lost.
	sum := map[bool]int{}
	for _, u := range units {
		switch u.Unit {
		case contract.UnitInputTokens, contract.UnitCachedInputTokens, contract.UnitAudioInputTokens, contract.UnitCachedAudioInputTokens:
			sum[true] += u.Quantity
		case contract.UnitOutputTokens, contract.UnitReasoningTokens, contract.UnitAudioOutputTokens:
			sum[false] += u.Quantity
		}
	}
	if sum[true] != 100 || sum[false] != 250 {
		t.Errorf("partition sums to %d/%d, want 100/250", sum[true], sum[false])
	}
	// Audio is never also reported by duration.
	for _, u := range units {
		if u.Unit == contract.UnitAudioInputMilliseconds || u.Unit == contract.UnitAudioOutputMilliseconds {
			t.Errorf("the same audio was reported as %s too", u.Unit)
		}
	}
	textOnly, ok := parse(`{"prompt_tokens":12,"completion_tokens":30,"completion_tokens_details":{"reasoning_tokens":4}}`).units()
	if !ok || !reflect.DeepEqual(textOnly, []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 12},
		{Unit: contract.UnitOutputTokens, Quantity: 26}, {Unit: contract.UnitReasoningTokens, Quantity: 4},
	}) {
		t.Errorf("text-only usage = %+v, %t", textOnly, ok)
	}
	for name, raw := range map[string]string{
		"no prompt count":              `{"completion_tokens":3}`,
		"no completion count":          `{"prompt_tokens":3}`,
		"audio beyond the prompt":      `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"audio_tokens":11}}`,
		"audio beyond the completion":  `{"prompt_tokens":10,"completion_tokens":1,"completion_tokens_details":{"audio_tokens":2}}`,
		"cached audio beyond audio":    `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":3,"audio_tokens":1,"cached_tokens_details":{"audio_tokens":2}}}`,
		"cached audio beyond cached":   `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":1,"audio_tokens":4,"cached_tokens_details":{"audio_tokens":2}}}`,
		"negative completion":          `{"prompt_tokens":10,"completion_tokens":-1}`,
		"cached and audio overlap too": `{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":6,"audio_tokens":6}}`,
	} {
		if units, ok := parse(raw).units(); ok {
			t.Errorf("%s: accepted as %+v; an inconsistent report must not be clamped into a bill", name, units)
		}
	}
}
