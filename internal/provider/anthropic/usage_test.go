package anthropic

import (
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
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
	//   correct:  input 7, cache write 2, cached 3                        = 12
	//   nested:   input 7 alone, 3 cached                                 = 10  (loses the cache write)
	//   openai:   input 7 − 3 cached = 4, 3 cached                        =  7  (subtracts what was never included)
	//
	// Only the first sums to the 12 prompt tokens the request consumed.
	if got[contract.UnitInputTokens] != 7 {
		t.Errorf("input_tokens is %d, expected 7: it excludes the cache read and the cache write", got[contract.UnitInputTokens])
	}
	if got[contract.UnitCachedInputTokens] != 3 {
		t.Errorf("cached_input_tokens is %d, expected 3", got[contract.UnitCachedInputTokens])
	}
	if got[contract.UnitCacheWriteInputTokens] != 2 {
		t.Errorf("cache_write_input_tokens is %d, expected 2", got[contract.UnitCacheWriteInputTokens])
	}
	if sum := got[contract.UnitInputTokens] + got[contract.UnitCachedInputTokens] + got[contract.UnitCacheWriteInputTokens]; sum != 12 {
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

// TestCacheWriteTokensAreTheirOwnUnit pins the mapping of a cache WRITE.
//
// A write is an input token this provider charges a premium for (1.25x for the
// default 5-minute TTL); a read is one it charges a tenth of the input rate
// for. Reporting a write under `cached_input_tokens` would price the most
// expensive input tokens in the request at the cheapest rate on the card, and
// leaving it in `input_tokens` loses the premium. The adapter reports it as
// `cache_write_input_tokens`; folding it back for a grant that does not price
// the unit is the executor's job (kaana.FoldCacheWrite), tested there.
func TestCacheWriteTokensAreTheirOwnUnit(t *testing.T) {
	meter := &usageMeter{}
	meter.absorb(&usage{
		InputTokens:              intOf(4),
		CacheCreationInputTokens: intOf(6),
		CacheReadInputTokens:     intOf(0),
		OutputTokens:             intOf(2),
	}, true)

	got := reported(mustUnits(t, meter))
	if got[contract.UnitInputTokens] != 4 {
		t.Errorf("input_tokens is %d, expected 4 (the write is not input)", got[contract.UnitInputTokens])
	}
	if got[contract.UnitCacheWriteInputTokens] != 6 {
		t.Errorf("cache_write_input_tokens is %d, expected 6", got[contract.UnitCacheWriteInputTokens])
	}
	if _, present := got[contract.UnitCachedInputTokens]; present {
		t.Error("nothing was read from the cache, and cached_input_tokens was reported anyway")
	}
}

// No cache write measured: no unit. A zero-quantity unit would be priced (and
// rejected by Oxy for a grant that does not price it) for nothing.
func TestNoCacheWriteReportsNoCacheWriteUnit(t *testing.T) {
	for name, creation := range map[string]*int{"absent": nil, "zero": intOf(0)} {
		meter := &usageMeter{}
		meter.absorb(&usage{InputTokens: intOf(4), CacheCreationInputTokens: creation, OutputTokens: intOf(2)}, true)
		if _, present := reported(mustUnits(t, meter))[contract.UnitCacheWriteInputTokens]; present {
			t.Errorf("%s: a cache_write_input_tokens unit was reported for a request that wrote nothing", name)
		}
	}
}

// The single unit is the default 5-minute write. Tokens the provider says it
// wrote with the 1-hour TTL (2x) stay in input_tokens, as every write was
// metered before the unit existed, and only the remainder is the unit; with no
// per-TTL breakdown the whole total is the default TTL.
func TestOnlyTheFiveMinuteWriteIsTheCacheWriteUnit(t *testing.T) {
	cases := map[string]struct {
		breakdown            *cacheCreation
		wantInput, wantWrite int
	}{
		"no breakdown":     {nil, 4, 10},
		"all five minutes": {&cacheCreation{Ephemeral1hInputTokens: intOf(0)}, 4, 10},
		"mixed":            {&cacheCreation{Ephemeral1hInputTokens: intOf(3)}, 7, 7},
		"all one hour":     {&cacheCreation{Ephemeral1hInputTokens: intOf(10)}, 14, 0},
		"more than total":  {&cacheCreation{Ephemeral1hInputTokens: intOf(99)}, 14, 0},
	}
	for name, tc := range cases {
		meter := &usageMeter{}
		meter.absorb(&usage{InputTokens: intOf(4), CacheCreationInputTokens: intOf(10), CacheCreation: tc.breakdown, OutputTokens: intOf(2)}, true)
		got := reported(mustUnits(t, meter))
		if got[contract.UnitInputTokens] != tc.wantInput || got[contract.UnitCacheWriteInputTokens] != tc.wantWrite {
			t.Errorf("%s: input_tokens %d, cache_write_input_tokens %d; want %d and %d", name, got[contract.UnitInputTokens], got[contract.UnitCacheWriteInputTokens], tc.wantInput, tc.wantWrite)
		}
		if got[contract.UnitInputTokens]+got[contract.UnitCacheWriteInputTokens] != 14 {
			t.Errorf("%s: the units do not sum to the 14 prompt tokens", name)
		}
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
