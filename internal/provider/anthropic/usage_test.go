package anthropic

import (
	"testing"

	"github.com/OxyHQ/Pensara/internal/contract"
)

// The usage tests are the ones worth reading twice. Every failure they cover is
// silent — the numbers stay small, non-negative and plausible — and every one of
// them is money.

func reported(units []contract.UsageQuantity) map[contract.UsageUnit]int {
	byUnit := make(map[contract.UsageUnit]int, len(units))
	for _, quantity := range units {
		byUnit[quantity.Unit] = quantity.Quantity
	}
	return byUnit
}

func intOf(value int) *int { return &value }

// completeUsage is one physical request as this provider reports it: 12 prompt
// tokens, 3 of them served from the cache and 2 of them written to it, and 5
// output tokens of which 2 were thinking.
func completeUsage() *usage {
	return &usage{
		InputTokens:              intOf(7),
		CacheCreationInputTokens: intOf(2),
		CacheReadInputTokens:     intOf(3),
		OutputTokens:             intOf(5),
		OutputTokensDetails:      &outputTokensDetails{ThinkingTokens: intOf(2)},
	}
}

// TestUsageUnitsPartitionThePhysicalRequest pins the arithmetic that decides
// what a customer is charged.
//
// Both children are non-zero, deliberately: with either at zero, the correct
// reading and both wrong ones agree, and the test would pass while measuring
// nothing.
func TestUsageUnitsPartitionThePhysicalRequest(t *testing.T) {
	meter := &usageMeter{}
	meter.absorb(completeUsage(), true)

	units, source := meter.units()
	if source != contract.UsageProviderReported {
		t.Errorf("the provider reported a final count; the source says %q", source)
	}
	got := reported(units)

	// The three readings of the same four numbers, and what each would charge.
	//
	//   correct:  input 7 + cache write 2 = 9 uncached, 3 cached          = 12
	//   nested:   input 7 alone, 3 cached                                 = 10  (loses the cache write)
	//   openai:   input 7 − 3 cached = 4, 3 cached                        =  7  (subtracts what was never included)
	//
	// Only the first sums to the 12 prompt tokens the request consumed.
	if got[contract.UnitInputTokens] != 9 {
		t.Errorf("input_tokens is %d, expected 9 (7 uncached + 2 written to the cache)", got[contract.UnitInputTokens])
	}
	if got[contract.UnitCachedInputTokens] != 3 {
		t.Errorf("cached_input_tokens is %d, expected 3", got[contract.UnitCachedInputTokens])
	}
	if sum := got[contract.UnitInputTokens] + got[contract.UnitCachedInputTokens]; sum != 12 {
		t.Errorf("the input units sum to %d and the request consumed 12 prompt tokens", sum)
	}

	// The other direction: `output_tokens` DOES include the thinking tokens, so
	// this half needs the subtraction the half above must not have.
	if got[contract.UnitOutputTokens] != 3 {
		t.Errorf("output_tokens is %d, expected 3 (5 generated, 2 of them reasoning)", got[contract.UnitOutputTokens])
	}
	if got[contract.UnitReasoningTokens] != 2 {
		t.Errorf("reasoning_tokens is %d, expected 2", got[contract.UnitReasoningTokens])
	}
	if sum := got[contract.UnitOutputTokens] + got[contract.UnitReasoningTokens]; sum != 5 {
		t.Errorf("the output units sum to %d and the request produced 5 output tokens", sum)
	}
	if got[contract.UnitRequests] != 1 {
		t.Errorf("the meter reports %d requests", got[contract.UnitRequests])
	}
}

// TestCacheWriteTokensAreInputTokensAndNotCachedOnes pins the one mapping the
// contract has no unit for.
//
// A cache WRITE is an input token the model processed and this provider charges
// a premium for; a cache READ is one it charges a tenth of the input rate for.
// Reporting a write under `cached_input_tokens` would price the most expensive
// input tokens in the request at the cheapest rate on the card.
func TestCacheWriteTokensAreInputTokensAndNotCachedOnes(t *testing.T) {
	meter := &usageMeter{}
	meter.absorb(&usage{
		InputTokens:              intOf(4),
		CacheCreationInputTokens: intOf(6),
		CacheReadInputTokens:     intOf(0),
		OutputTokens:             intOf(2),
	}, true)

	got := reported(mustUnits(t, meter))
	if got[contract.UnitInputTokens] != 10 {
		t.Errorf("input_tokens is %d, expected 10 (4 uncached + 6 written to the cache)", got[contract.UnitInputTokens])
	}
	if _, present := got[contract.UnitCachedInputTokens]; present {
		t.Error("nothing was read from the cache, and cached_input_tokens was reported anyway")
	}
}

// TestTheOutputCountIsCumulativeAndNotAdditive covers the failure this
// protocol's two-event usage invites.
//
// `message_start` reports the output tokens generated so far and the final
// `message_delta` reports the total for the request. An adapter that added them
// would overcharge every single request by the opening count — small, constant
// and completely invisible on any one receipt.
func TestTheOutputCountIsCumulativeAndNotAdditive(t *testing.T) {
	meter := &usageMeter{}
	meter.absorb(&usage{InputTokens: intOf(7), CacheReadInputTokens: intOf(3), OutputTokens: intOf(1)}, false)
	meter.absorb(&usage{OutputTokens: intOf(5), OutputTokensDetails: &outputTokensDetails{ThinkingTokens: intOf(2)}}, true)

	got := reported(mustUnits(t, meter))
	if sum := got[contract.UnitOutputTokens] + got[contract.UnitReasoningTokens]; sum != 5 {
		t.Errorf("the output units sum to %d; the provider's last cumulative count was 5, and 6 is what adding the two events produces", sum)
	}
	// The later event says nothing about the input side, and must not be read
	// as saying it was zero.
	if got[contract.UnitInputTokens] != 7 {
		t.Errorf("input_tokens is %d after an event that omitted it, expected the 7 the first event reported", got[contract.UnitInputTokens])
	}
	if got[contract.UnitCachedInputTokens] != 3 {
		t.Errorf("cached_input_tokens is %d after an event that omitted it, expected 3", got[contract.UnitCachedInputTokens])
	}
}

// TestAStreamCutShortReportsAnExactInputAndAnEstimatedTotal is the settlement
// case: what a cancelled request is charged.
func TestAStreamCutShortReportsAnExactInputAndAnEstimatedTotal(t *testing.T) {
	meter := &usageMeter{}
	// Only the opening event arrived, which is what a cancellation after the
	// first chunk looks like.
	meter.absorb(&usage{InputTokens: intOf(7), CacheReadInputTokens: intOf(3), OutputTokens: intOf(1)}, false)

	units, source := meter.units()
	// The output count here is provisional, so the whole measurement is an
	// estimate: a provisional number reported as the provider's own is one
	// nobody can reconcile against an invoice later.
	if source != contract.UsageEstimated {
		t.Errorf("a stream cut off before its final message_delta reports source %q", source)
	}
	got := reported(units)
	if got[contract.UnitInputTokens]+got[contract.UnitCachedInputTokens] != 10 {
		t.Errorf("the input units sum to %d; the provider had already reported 10 prompt tokens exactly",
			got[contract.UnitInputTokens]+got[contract.UnitCachedInputTokens])
	}
	if got[contract.UnitRequests] != 1 {
		t.Error("a cancelled request that reached the provider still consumed one request")
	}
}

// TestNoUsageReportedIsNotZeroUsage separates the two states a receipt cannot
// afford to confuse.
func TestNoUsageReportedIsNotZeroUsage(t *testing.T) {
	meter := &usageMeter{}
	units, source := meter.units()
	if len(units) != 0 {
		t.Errorf("a provider that reported nothing produced %d units: a metered request that consumed nothing and an unmetered one are different settlements", len(units))
	}
	if source != contract.UsageEstimated {
		t.Errorf("a provider that reported nothing has source %q", source)
	}

	// The control: a provider that reported an explicit zero DID report.
	zeroed := &usageMeter{}
	zeroed.absorb(&usage{InputTokens: intOf(0), OutputTokens: intOf(0)}, true)
	units, source = zeroed.units()
	if len(units) == 0 {
		t.Error("a provider that reported zeros produced no units at all")
	}
	if source != contract.UsageProviderReported {
		t.Errorf("a provider that reported zeros has source %q", source)
	}
}

func mustUnits(t *testing.T, meter *usageMeter) []contract.UsageQuantity {
	t.Helper()
	units, _ := meter.units()
	if len(units) == 0 {
		t.Fatal("the meter reported no units")
	}
	return units
}
