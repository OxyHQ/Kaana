package anthropic

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// TestEveryAttributedAnthropicModelHasAReviewedParameterStatement keeps the
// per-model table in step with the attribution: a newly attributed Claude id
// is reviewed here rather than published with no statement unnoticed.
func TestEveryAttributedAnthropicModelHasAReviewedParameterStatement(t *testing.T) {
	raw, err := os.ReadFile("../../../configs/model-attribution.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Attribution map[contract.ProviderSlug]map[string]string `json:"attribution"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for id := range file.Attribution[Slug] {
		if _, stated := AcceptedParameters(id); !stated {
			t.Errorf("anthropic/%s is attributed and has no reviewed parameter statement", id)
		}
	}
	const want = 13
	if len(file.Attribution[Slug]) != want || len(refusedByModel) != want {
		t.Errorf("%d attributed Anthropic ids and %d reviewed rows, want exactly %d of each", len(file.Attribution[Slug]), len(refusedByModel), want)
	}
}

func TestAcceptedParametersStateWhatAnthropicDocumentsEachModelRejects(t *testing.T) {
	forced := []provider.RequestParameter{provider.ParameterToolChoiceFunction, provider.ParameterToolChoiceRequired}
	sampling := []provider.RequestParameter{provider.ParameterTemperature, provider.ParameterTopP}
	for _, c := range []struct {
		id                   string
		takesForced, samples bool
	}{
		{"claude-fable-5-1", false, false},
		{"claude-opus-5-5", false, false},
		{"claude-sonnet-5-5", false, false},
		{"claude-fable-5", true, false},
		{"claude-opus-4-7", true, false},
		{"claude-haiku-5-5", true, false},
		{"claude-opus-4-6", true, true},
		{"claude-haiku-4-5-20251001", true, true},
	} {
		accepted, stated := AcceptedParameters(c.id)
		if !stated {
			t.Fatalf("%s: no statement", c.id)
		}
		if err := provider.ValidateParameterSet(accepted); err != nil {
			t.Errorf("%s: %v", c.id, err)
		}
		if !slices.Equal(provider.CanonicalParameterSet(accepted), accepted) {
			t.Errorf("%s: the statement is not canonical: %v", c.id, accepted)
		}
		// Every model takes tools, `none`, and the controls the Messages API
		// always carries.
		for _, parameter := range []provider.RequestParameter{provider.ParameterMaxOutputTokens, provider.ParameterStopSequences, provider.ParameterTools, provider.ParameterToolChoice, provider.ParameterToolChoiceNone} {
			if !slices.Contains(accepted, parameter) {
				t.Errorf("%s does not accept %s: %v", c.id, parameter, accepted)
			}
		}
		for _, parameter := range forced {
			if slices.Contains(accepted, parameter) != c.takesForced {
				t.Errorf("%s accepts %s = %t, want %t", c.id, parameter, !c.takesForced, c.takesForced)
			}
		}
		for _, parameter := range sampling {
			if slices.Contains(accepted, parameter) != c.samples {
				t.Errorf("%s accepts %s = %t, want %t", c.id, parameter, !c.samples, c.samples)
			}
		}
		// Translate refuses what the protocol has no field for, so no
		// statement may name it.
		for _, parameter := range []provider.RequestParameter{provider.ParameterFrequencyPenalty, provider.ParameterPresencePenalty, provider.ParameterSeed, provider.ParameterResponseFormat} {
			if slices.Contains(accepted, parameter) {
				t.Errorf("%s accepts %s, which this adapter refuses", c.id, parameter)
			}
		}
	}
	if _, stated := AcceptedParameters("claude-unreviewed-9"); stated {
		t.Error("an unreviewed model id has a statement")
	}
}

func opusRoute(accepted *[]provider.RequestParameter) provider.Route {
	route := testRoute()
	route.UpstreamModelID = "claude-opus-5-5"
	route.AcceptedParameters = accepted
	return route
}

func withTools(request *contract.Request, choice *contract.ToolChoice) *contract.Request {
	request.Tools = []contract.ToolDefinition{{Type: "function", Name: "lookup", Parameters: map[string]any{"type": "object"}}}
	request.ToolChoice = choice
	return request
}

func mode(value contract.ToolChoiceMode) *contract.ToolChoice {
	return &contract.ToolChoice{Mode: &value}
}

// TestAForcedToolChoiceNeverReachesAModelThatRejectsIt: Claude Opus 5.5
// answers `any`/`tool` with a terminal 400, so Translate refuses them before
// anything is sent — even on a route whose published set is unknown — and
// still sends `auto` and `none`.
func TestAForcedToolChoiceNeverReachesAModelThatRejectsIt(t *testing.T) {
	adapter := newTestAdapter(t)
	published, _ := AcceptedParameters("claude-opus-5-5")
	for name, route := range map[string]provider.Route{"published set": opusRoute(&published), "unknown set": opusRoute(nil)} {
		for _, forced := range []*contract.ToolChoice{mode(contract.ToolChoiceRequired), {Function: &contract.ToolChoiceFunction{Name: "lookup"}}} {
			_, err := adapter.Translate(withTools(baseRequest("req_forced"), forced), route)
			var unsupported provider.ErrUnsupported
			if !errors.As(err, &unsupported) || unsupported.Code != contract.CodeInvalidRequest || unsupported.Param != "toolChoice" || unsupported.Code.Retryable() {
				t.Errorf("%s: a forced tool choice was not refused on toolChoice: %v", name, err)
			}
		}
		for want, choice := range map[string]*contract.ToolChoice{"auto": mode(contract.ToolChoiceAuto), "none": mode(contract.ToolChoiceNone)} {
			call, err := adapter.Translate(withTools(baseRequest("req_unforced"), choice), route)
			if err != nil {
				t.Errorf("%s: tool choice %s was refused: %v", name, want, err)
				continue
			}
			var body messagesRequest
			if err := json.Unmarshal(call.Body, &body); err != nil || body.ToolChoice == nil || body.ToolChoice.Type != want {
				t.Errorf("%s: tool choice %s was sent as %+v (%v)", name, want, body.ToolChoice, err)
			}
		}
	}

	// Control: the same forced choice on a model that takes it is sent as
	// this protocol spells it.
	route := testRoute()
	route.UpstreamModelID = "claude-opus-4-8"
	call, err := adapter.Translate(withTools(baseRequest("req_forced_ok"), mode(contract.ToolChoiceRequired)), route)
	if err != nil {
		t.Fatalf("control: Opus 4.8 takes a forced choice and it was refused: %v", err)
	}
	var body messagesRequest
	if err := json.Unmarshal(call.Body, &body); err != nil || body.ToolChoice == nil || body.ToolChoice.Type != "any" {
		t.Errorf("control: required was sent as %+v (%v)", body.ToolChoice, err)
	}
}

func TestSamplingIsRefusedWhereAnthropicRejectsItAndThePublishedSetIsHonoured(t *testing.T) {
	adapter := newTestAdapter(t)
	temperature := 0.2
	request := baseRequest("req_temperature")
	request.Sampling.Temperature = &temperature
	_, err := adapter.Translate(request, opusRoute(nil))
	var unsupported provider.ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Param != "sampling.temperature" {
		t.Errorf("a temperature Claude Opus 5.5 rejects was translated: %v", err)
	}

	// A model id with no reviewed statement is held to its route's published
	// set alone: a set lacking `reasoning.effort` refuses an effort, an
	// unknown one refuses nothing.
	effort := baseRequest("req_effort")
	effort.Reasoning = &contract.ReasoningParameters{Effort: contract.ReasoningEffortLow}
	withoutEffort := []provider.RequestParameter{provider.ParameterMaxOutputTokens, provider.ParameterTools}
	route := testRoute()
	route.AcceptedParameters = &withoutEffort
	if _, err := adapter.Translate(effort, route); !errors.As(err, &unsupported) || unsupported.Param != "reasoning.effort" {
		t.Errorf("an effort the route's set lacks was translated: %v", err)
	}
	if _, err := adapter.Translate(effort, testRoute()); err != nil {
		t.Errorf("control: an unknown set refused an effort: %v", err)
	}
}
