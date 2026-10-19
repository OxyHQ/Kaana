package publicapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// A decisions request without a model asks Oxy for no target, is served on
// the revision of the line the grant names that Kaana serves, and says which
// revision answered. A named model must be pinned. The estimate the grant is
// sized from carries the framing a deployment that may answer bills on every
// request (its rate card's requestInputTokens, 300 for dep_judge).
func TestDecisionsServeTheGrantedProfile(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	instructions := "Judge the post."
	input := contract.DecisionInput{State: "A post.", Instructions: &instructions, Questions: []contract.DecisionQuestion{{ID: "spam", Kind: "noul", Question: "Is it spam?"}}}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	wantEstimate := (max(len(body), input.Budget().Total)+3)/4 + 300
	h.oxy.steps = func(request contract.InferenceAuthorizationRequest) []contract.GrantStep {
		if request.Operation != contract.GrantDecisions || request.Target != nil {
			t.Errorf("a decisions request without a model asked for %s %+v", request.Operation, request.Target)
		}
		if estimate := request.Facts.EstimatedInputTokens; estimate == nil || *estimate != wantEstimate {
			t.Errorf("estimatedInputTokens is not %d", wantEstimate)
		}
		line := judgeModel.ModelID()
		return []contract.GrantStep{{Target: contract.GrantTarget{Kind: contract.GrantTargetModel, ModelLine: &line},
			Limits: contract.GrantStepLimits{AdmittedModelLines: []contract.AdmittedModelLine{{ModelLine: line, PriceVersionID: "price-1"}}}}}
	}
	question := map[string]any{"id": "spam", "kind": "noul", "question": "Is it spam?"}
	response := h.post("/v1/decisions", validKey, body)
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
