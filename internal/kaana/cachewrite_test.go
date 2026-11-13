package kaana_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// cacheWritingAdapter reports a request that wrote 6 tokens to a prompt cache:
// 4 input tokens, the 6 written, 2 read, 3 output.
func cacheWritingAdapter() *scriptedAdapter {
	units := []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1},
		{Unit: contract.UnitInputTokens, Quantity: 4},
		{Unit: contract.UnitCachedInputTokens, Quantity: 2},
		{Unit: contract.UnitCacheWriteInputTokens, Quantity: 6},
		{Unit: contract.UnitOutputTokens, Quantity: 3},
	}
	return &scriptedAdapter{stream: func(_ context.Context, call *provider.Call, out provider.Emitter) (provider.Outcome, error) {
		if err := out.Start(call.Route.ModelReference, time.Now()); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Delta(0, contract.ChannelOutputText, "hello"); err != nil {
			return provider.Outcome{}, err
		}
		if err := out.Usage(units, contract.UsageProviderReported); err != nil {
			return provider.Outcome{}, err
		}
		return provider.Outcome{Units: units, UsageSource: contract.UsageProviderReported, FinishReason: contract.FinishStop}, nil
	}}
}

func unitsOf(units []contract.UsageQuantity) map[contract.UsageUnit]int {
	byUnit := make(map[contract.UsageUnit]int, len(units))
	for _, quantity := range units {
		byUnit[quantity.Unit] += quantity.Quantity
	}
	return byUnit
}

// everywhereUnitsAppear collects the units the customer's stream, the report
// settlement carries and the operator cost record each hold.
func everywhereUnitsAppear(t *testing.T, ctx context.Context) (stream, report, cost map[contract.UsageUnit]int) {
	t.Helper()
	h := harness{deployments: oneDeployment, adapters: []provider.Adapter{cacheWritingAdapter()}}
	var events []contract.StreamEvent
	result := h.build(t).Execute(ctx, baseRequest(), func(event contract.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if result.Failure != nil {
		t.Fatal(result.Failure)
	}
	for _, event := range events {
		if usage, ok := event.(*contract.StreamUsageEvent); ok {
			stream = unitsOf(usage.Units)
		}
	}
	if stream == nil {
		t.Fatal("no usage event reached the customer")
	}
	if len(result.UpstreamCost.Attempts) != 1 {
		t.Fatalf("%d attempts in the cost record", len(result.UpstreamCost.Attempts))
	}
	return stream, unitsOf(result.Report.Units), unitsOf(result.UpstreamCost.Attempts[0].Units)
}

// The unpriced branch: a request whose grant does not price the unit (or says
// nothing about prices) reports exactly what it reported before the unit
// existed, on every path that leaves the process. Mutation: a fold that always
// splits fails this test.
func TestAnUnpricedGrantFoldsTheCacheWriteIntoInputTokens(t *testing.T) {
	notPriced := kaana.CacheWritePriced(func(provider.Route) bool { return false })
	for name, ctx := range map[string]context.Context{
		"no price information": context.Background(),
		"a grant without it":   kaana.WithCacheWritePriced(context.Background(), notPriced),
	} {
		t.Run(name, func(t *testing.T) {
			stream, report, cost := everywhereUnitsAppear(t, ctx)
			for path, got := range map[string]map[contract.UsageUnit]int{"the stream": stream, "the report": report, "the cost record": cost} {
				if _, present := got[contract.UnitCacheWriteInputTokens]; present {
					t.Errorf("%s carries cache_write_input_tokens for a grant that does not price it: Oxy would reject the settlement", path)
				}
				if got[contract.UnitInputTokens] != 10 {
					t.Errorf("%s: input_tokens is %d, expected 10 (4 + the 6 written)", path, got[contract.UnitInputTokens])
				}
				if got[contract.UnitCachedInputTokens] != 2 || got[contract.UnitOutputTokens] != 3 {
					t.Errorf("%s: another unit changed: %v", path, got)
				}
			}
		})
	}
}

// The priced branch: the grant's price list carries the unit, so the split
// reaches the customer, the settlement and the cost record. Mutation: a fold
// that never splits fails this test.
func TestAPricedGrantKeepsTheCacheWriteAsItsOwnUnit(t *testing.T) {
	var asked []contract.ModelReference
	priced := kaana.CacheWritePriced(func(route provider.Route) bool {
		asked = append(asked, route.ModelReference)
		return true
	})
	stream, report, cost := everywhereUnitsAppear(t, kaana.WithCacheWritePriced(context.Background(), priced))
	for path, got := range map[string]map[contract.UsageUnit]int{"the stream": stream, "the report": report, "the cost record": cost} {
		if got[contract.UnitCacheWriteInputTokens] != 6 {
			t.Errorf("%s: cache_write_input_tokens is %d, expected 6", path, got[contract.UnitCacheWriteInputTokens])
		}
		if got[contract.UnitInputTokens] != 4 {
			t.Errorf("%s: input_tokens is %d, expected 4 (the write is not input)", path, got[contract.UnitInputTokens])
		}
	}
	if len(asked) == 0 || asked[0] != "stub/model@2026-05-01" {
		t.Errorf("the price question named %v, expected the route's model reference", asked)
	}
}

func TestFoldCacheWrite(t *testing.T) {
	split := []contract.UsageQuantity{
		{Unit: contract.UnitRequests, Quantity: 1},
		{Unit: contract.UnitInputTokens, Quantity: 4},
		{Unit: contract.UnitCacheWriteInputTokens, Quantity: 6},
	}
	if got := kaana.FoldCacheWrite(split, true); !reflect.DeepEqual(got, split) {
		t.Errorf("a priced request changed: %v", got)
	}
	want := []contract.UsageQuantity{{Unit: contract.UnitRequests, Quantity: 1}, {Unit: contract.UnitInputTokens, Quantity: 10}}
	if got := kaana.FoldCacheWrite(split, false); !reflect.DeepEqual(got, want) {
		t.Errorf("folded = %v, want %v", got, want)
	}
	if split[1].Quantity != 4 || len(split) != 3 {
		t.Error("folding modified its input")
	}

	// A report with no input_tokens entry still keeps the tokens.
	onlyWrite := []contract.UsageQuantity{{Unit: contract.UnitCacheWriteInputTokens, Quantity: 6}}
	if got := kaana.FoldCacheWrite(onlyWrite, false); !reflect.DeepEqual(got, []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 6}}) {
		t.Errorf("folded = %v", got)
	}

	// Nothing to fold: the same list comes back untouched.
	plain := []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 4}}
	if got := kaana.FoldCacheWrite(plain, false); !reflect.DeepEqual(got, plain) {
		t.Errorf("a request with no write changed: %v", got)
	}
}
