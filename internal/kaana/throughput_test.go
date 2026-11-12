package kaana

import (
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

func TestOutputTokensPerSecondReadsOnlyAReadingWorthTrusting(t *testing.T) {
	units := func(output int) []contract.UsageQuantity {
		return []contract.UsageQuantity{{Unit: contract.UnitInputTokens, Quantity: 999}, {Unit: contract.UnitOutputTokens, Quantity: output}}
	}
	measured := func(latency, firstOutput time.Duration) providercost.AttemptTelemetry {
		return providercost.AttemptTelemetry{Latency: latency, TimeToFirstOutput: firstOutput}
	}
	if got := outputTokensPerSecond(units(200), measured(3*time.Second, time.Second)); got != 100 {
		t.Errorf("200 tokens over the 2 s after the first output is 100/s, got %v", got)
	}
	for name, c := range map[string]struct {
		units     []contract.UsageQuantity
		telemetry providercost.AttemptTelemetry
	}{
		"too few tokens":       {units(8), measured(3*time.Second, time.Second)},
		"no first output":      {units(200), measured(3*time.Second, 0)},
		"a window under 100ms": {units(200), measured(1050*time.Millisecond, time.Second)},
		"no output units":      {nil, measured(3*time.Second, time.Second)},
	} {
		if got := outputTokensPerSecond(c.units, c.telemetry); got != 0 {
			t.Errorf("%s gave %v, want 0", name, got)
		}
	}
}
