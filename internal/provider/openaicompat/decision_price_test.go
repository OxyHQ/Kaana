package openaicompat

import (
	"encoding/json"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"testing"
)

func TestDecisionPolicyExactFractionalMaxPrice(t *testing.T) {
	prompt, err := providercost.ParseDecisionTokenPrice("0.042")
	if err != nil {
		t.Fatal(err)
	}
	body := systemOneRequest{Provider: &decisionProviderPolicy{MaxPrice: providercost.DecisionPriceLimit{Prompt: prompt}}}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Provider struct {
			MaxPrice struct {
				Prompt     json.Number
				Completion json.Number
			} `json:"max_price"`
		}
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider.MaxPrice.Prompt.String() != "0.042" || decoded.Provider.MaxPrice.Completion.String() != "0" {
		t.Fatalf("fractional numeric ceiling changed: %s", encoded)
	}
	body.Provider.MaxPrice = providercost.DecisionPriceLimit{}
	encoded, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider.MaxPrice.Prompt.String() != "0" || decoded.Provider.MaxPrice.Completion.String() != "0" {
		t.Fatalf("default spend ceiling changed: %s", encoded)
	}
}
