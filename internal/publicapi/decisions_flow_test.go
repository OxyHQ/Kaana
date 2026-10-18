package publicapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// A decisions request without a model asks Oxy for no target, is served on
// the revision of the line the grant names that Kaana serves, and says which
// revision answered. A named model must be pinned.
func TestDecisionsServeTheGrantedProfile(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.oxy.steps = func(request contract.InferenceAuthorizationRequest) []contract.GrantStep {
		if request.Operation != contract.GrantDecisions || request.Target != nil {
			t.Errorf("a decisions request without a model asked for %s %+v", request.Operation, request.Target)
		}
		line := judgeModel.ModelID()
		return []contract.GrantStep{{Target: contract.GrantTarget{Kind: contract.GrantTargetModel, ModelLine: &line},
			Limits: contract.GrantStepLimits{AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: line, PriceVersionID: "price-1"}}}}}
	}
	question := map[string]any{"id": "spam", "kind": "noul", "question": "Is it spam?"}
	response := h.post("/v1/decisions", validKey, map[string]any{"state": "A post.", "questions": []any{question}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, response.Body)
	}
	var success struct {
		Model contract.ModelReference   `json:"model"`
		Data  []contract.DecisionAnswer `json:"data"`
	}
	if err := json.Unmarshal(readAll(t, response), &success); err != nil {
		t.Fatal(err)
	}
	if success.Model != judgeModel || len(success.Data) != 1 || success.Data[0].ID != "spam" {
		t.Fatalf("decisions = %+v", success)
	}

	unpinned := h.post("/v1/decisions", validKey, map[string]any{"model": "stub/judge", "state": "A post.", "questions": []any{question}})
	if refusal := publicError(t, unpinned, http.StatusBadRequest); refusal.Param == nil || *refusal.Param != "model" {
		t.Fatalf("an unpinned model was not refused by name: %+v", refusal)
	}
}
