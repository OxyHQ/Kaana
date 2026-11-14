package publicapi

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// TestADefaultLineIsTheOnlyActiveOneOfItsKind holds
// capabilities.realtime.defaultKinds (contract set 8.16.0, Kaana ADR 0012 §4)
// on the catalogue Kaana loads: a line is the default only of a kind it holds,
// each kind once, and at most one ACTIVE line is the default of a kind. The
// controls start from the file's own realtime line, which parses unchanged
// and as one default beside a deprecated and a retired one.
func TestADefaultLineIsTheOnlyActiveOneOfItsKind(t *testing.T) {
	line := declaredRealtimeLine(t)
	defaulting := func(suffix string, kinds []contract.RealtimeSessionKind, status string) map[string]any {
		model := map[string]any{}
		for name, value := range line {
			model[name] = value
		}
		var id string
		if err := json.Unmarshal(line["modelId"], &id); err != nil {
			t.Fatal(err)
		}
		model["modelId"] = id + "-" + suffix
		var capabilities map[string]any
		if err := json.Unmarshal(line["capabilities"], &capabilities); err != nil {
			t.Fatal(err)
		}
		realtime := capabilities["realtime"].(map[string]any)
		realtime["defaultKinds"] = kinds
		model["capabilities"] = capabilities
		model["deprecation"] = map[string]string{"status": status}
		return model
	}
	parse := func(models ...map[string]any) error {
		raw, err := json.Marshal(map[string]any{"schemaVersion": 1, "models": models})
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParsePublicCatalogue(raw)
		return err
	}
	conversation := []contract.RealtimeSessionKind{contract.RealtimeConversation}
	for _, accepted := range []struct {
		name   string
		models []map[string]any
	}{
		{"one active default line", []map[string]any{defaulting("a", conversation, "active")}},
		{"an active default beside a deprecated and a retired one", []map[string]any{
			defaulting("a", conversation, "active"), defaulting("b", conversation, "deprecated"), defaulting("c", conversation, "retired"),
		}},
	} {
		if err := parse(accepted.models...); err != nil {
			t.Errorf("%s was refused: %v", accepted.name, err)
		}
	}
	for _, refused := range []struct {
		name   string
		models []map[string]any
		reason string
	}{
		{"two active default lines of one kind", []map[string]any{
			defaulting("a", conversation, "active"), defaulting("b", conversation, "active"),
		}, "at most one active line"},
		{"a kind named twice", []map[string]any{
			defaulting("a", []contract.RealtimeSessionKind{contract.RealtimeConversation, contract.RealtimeConversation}, "active"),
		}, "twice"},
		{"a kind the line does not hold", []map[string]any{
			defaulting("a", []contract.RealtimeSessionKind{contract.RealtimeLive}, "active"),
		}, "does not hold"},
		{"no kind", []map[string]any{defaulting("a", []contract.RealtimeSessionKind{}, "active")}, "at least one"},
	} {
		if err := parse(refused.models...); err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Errorf("%s was accepted or refused for another reason: %v", refused.name, err)
		}
	}
}

// TestADefaultKindIsPublishedOnlyWhereItIsServed: render replaces
// capabilities.realtime with what the inventory serves and keeps the declared
// defaults of the kinds it serves, so a published default is always one of the
// line's published sessionKinds.
func TestADefaultKindIsPublishedOnlyWhereItIsServed(t *testing.T) {
	line := declaredRealtimeLine(t)
	var capabilities map[string]any
	if err := json.Unmarshal(line["capabilities"], &capabilities); err != nil {
		t.Fatal(err)
	}
	realtime := capabilities["realtime"].(map[string]any)
	realtime["sessionKinds"] = []contract.RealtimeSessionKind{contract.RealtimeConversation, contract.RealtimeLive}
	realtime["defaultKinds"] = []contract.RealtimeSessionKind{contract.RealtimeLive}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	line["capabilities"] = encoded
	raw, err := json.Marshal(map[string]any{"schemaVersion": 1, "models": []any{line}})
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ParsePublicCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	var id contract.ModelID
	if err := json.Unmarshal(line["modelId"], &id); err != nil {
		t.Fatal(err)
	}
	published := func(served ...contract.RealtimeSessionKind) []contract.RealtimeSessionKind {
		entry := inventory.CatalogueEntry{Model: id, Reference: contract.ModelReference(string(id) + "@observed-2026-11-14"),
			Realtime: &inventory.RealtimeObservation{Transports: []contract.RealtimeSessionTransport{contract.RealtimeWebSocket}, SessionKinds: served}}
		rendered, err := catalogue.models[id].render(entry, []string{"observed-2026-11-14"})
		if err != nil {
			t.Fatal(err)
		}
		var model struct {
			Capabilities struct {
				Realtime struct {
					SessionKinds []contract.RealtimeSessionKind `json:"sessionKinds"`
					DefaultKinds []contract.RealtimeSessionKind `json:"defaultKinds"`
				} `json:"realtime"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(rendered, &model); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(model.Capabilities.Realtime.SessionKinds, served) {
			t.Fatalf("rendered sessionKinds %v, the inventory serves %v", model.Capabilities.Realtime.SessionKinds, served)
		}
		return model.Capabilities.Realtime.DefaultKinds
	}
	if got := published(contract.RealtimeConversation, contract.RealtimeLive); !slices.Equal(got, []contract.RealtimeSessionKind{contract.RealtimeLive}) {
		t.Errorf("a line serving its default kind published defaultKinds %v", got)
	}
	if got := published(contract.RealtimeConversation); got != nil {
		t.Errorf("a line that does not serve its default kind published defaultKinds %v", got)
	}
}

// declaredRealtimeLine is the first realtime line of configs/public-catalogue.json.
func declaredRealtimeLine(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../../configs/public-catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for _, model := range file.Models {
		var capabilities struct {
			Realtime json.RawMessage `json:"realtime"`
		}
		if err := json.Unmarshal(model["capabilities"], &capabilities); err != nil {
			t.Fatal(err)
		}
		if capabilities.Realtime != nil {
			return model
		}
	}
	t.Fatal("configs/public-catalogue.json declares no realtime line")
	return nil
}
