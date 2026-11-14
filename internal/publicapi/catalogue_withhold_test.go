package publicapi

import (
	"encoding/json"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// withhold leaves out what a reader's set lacks and only that: a list it
// empties takes the field CATALOGUE_ADDITIONS names with it (a line holding
// only `live` publishes no realtime capabilities to an 8.2.0 reader, a price
// list of new units no pricing), and a reader on the current set gets the
// model byte for byte.
func TestWithholdLeavesOutOnlyWhatTheReadersSetLacks(t *testing.T) {
	model := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"modelId": json.RawMessage(`"x/live"`),
			"capabilities": json.RawMessage(`{"inputModalities":["audio"],"unreported":["tools"],"maxContextTokens":32000,` +
				`"realtime":{"transports":["websocket"],"sessionKinds":["live"],"defaultKinds":["live"]}}`),
			"pricing":        json.RawMessage(`{"priceVersionId":"p1","currency":"USD","unitPrices":[{"unit":"cache_write_input_tokens","amount":"0.000001","per":1,"currency":"USD"}]}`),
			"pricePerMinute": json.RawMessage(`{"amount":"0.06","currency":"USD"}`),
		}
	}
	old := model()
	if err := withhold(old, contract.StatedContractSet("8.2.0")); err != nil {
		t.Fatal(err)
	}
	if _, present := old["pricing"]; present {
		t.Errorf("a price list of units 8.2.0 lacks was sent as %s", old["pricing"])
	}
	if got := string(old["capabilities"]); got != `{"inputModalities":["audio"],"maxContextTokens":32000}` {
		t.Errorf("an 8.2.0 reader was sent capabilities %s", got)
	}
	if string(old["pricePerMinute"]) != `{"amount":"0.06","currency":"USD"}` || string(old["modelId"]) != `"x/live"` {
		t.Errorf("a field no addition names was changed: %v", old)
	}

	// 8.4.0 has unreported and the unit, not live: the realtime line goes, its
	// defaultKinds with it, and nothing else.
	middle := model()
	if err := withhold(middle, contract.StatedContractSet("8.4.0")); err != nil {
		t.Fatal(err)
	}
	if got := string(middle["capabilities"]); got != `{"inputModalities":["audio"],"maxContextTokens":32000,"unreported":["tools"]}` {
		t.Errorf("an 8.4.0 reader was sent capabilities %s", got)
	}
	if string(middle["pricing"]) != string(model()["pricing"]) {
		t.Errorf("an 8.4.0 reader was sent pricing %s", middle["pricing"])
	}

	// A list that keeps a member keeps its holder.
	mixed := model()
	mixed["capabilities"] = json.RawMessage(`{"realtime":{"transports":["websocket"],"sessionKinds":["conversation","live"]}}`)
	if err := withhold(mixed, contract.StatedContractSet("8.15.0")); err != nil {
		t.Fatal(err)
	}
	if got := string(mixed["capabilities"]); got != `{"realtime":{"sessionKinds":["conversation"],"transports":["websocket"]}}` {
		t.Errorf("an 8.15.0 reader was sent capabilities %s", got)
	}

	current := model()
	if err := withhold(current, contract.CurrentContractSet); err != nil {
		t.Fatal(err)
	}
	for name, raw := range model() {
		if string(current[name]) != string(raw) {
			t.Errorf("a reader on %s was sent %s as %s, not %s", contract.ContractVersion, name, current[name], raw)
		}
	}
}
