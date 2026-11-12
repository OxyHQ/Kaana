package contract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The cache-write unit (contract set 8.4.0) is declared before anything emits
// it: it is a valid unit, it is the last of the list (Oxy compares the list in
// order), and a report carrying it survives the round trip.
func TestTheCacheWriteUnitRoundTrips(t *testing.T) {
	if !UnitCacheWriteInputTokens.Valid() {
		t.Fatal("cache_write_input_tokens is not a valid unit")
	}
	if got := usageUnitValues[len(usageUnitValues)-1]; got != UnitCacheWriteInputTokens {
		t.Errorf("the unit list ends in %q, want cache_write_input_tokens last", got)
	}

	report := failedReportWithNothingMeasured()
	report.Outcome = OutcomeCompleted
	report.Units = []UsageQuantity{
		{Unit: UnitInputTokens, Quantity: 10},
		{Unit: UnitCacheWriteInputTokens, Quantity: 1200},
	}
	report.UsageSource = UsageProviderReported

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if !strings.Contains(string(encoded), `{"unit":"cache_write_input_tokens","quantity":1200}`) {
		t.Errorf("the unit did not survive the encoding:\n%s", encoded)
	}
	var decoded UsageReport
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !reflect.DeepEqual(report, decoded) {
		t.Errorf("the report did not round-trip:\n sent %+v\n back %+v", report, decoded)
	}
}
