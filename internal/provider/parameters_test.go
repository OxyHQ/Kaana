package provider

import (
	"slices"
	"testing"

	"github.com/OxyHQ/Kaana/internal/contract"
)

func TestAToolChoiceNeedsItsValueAndAutoNeedsNothing(t *testing.T) {
	mode := func(value contract.ToolChoiceMode) *contract.ToolChoice { return &contract.ToolChoice{Mode: &value} }
	for name, c := range map[string]struct {
		choice *contract.ToolChoice
		want   []RequestParameter
	}{
		"absent":   {nil, nil},
		"auto":     {mode(contract.ToolChoiceAuto), nil},
		"none":     {mode(contract.ToolChoiceNone), []RequestParameter{ParameterToolChoice, ParameterToolChoiceNone}},
		"required": {mode(contract.ToolChoiceRequired), []RequestParameter{ParameterToolChoice, ParameterToolChoiceRequired}},
		"function": {&contract.ToolChoice{Function: &contract.ToolChoiceFunction{Name: "lookup"}}, []RequestParameter{ParameterToolChoice, ParameterToolChoiceFunction}},
	} {
		if got := ToolChoiceParameters(c.choice); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestASetIsReadCanonically(t *testing.T) {
	all := []RequestParameter{ParameterToolChoice, ParameterToolChoiceFunction, ParameterToolChoiceNone, ParameterToolChoiceRequired, ParameterTools}
	for name, c := range map[string]struct{ in, want []RequestParameter }{
		// What a provider says when its list names only `tool_choice`, and
		// every set stated before values were distinguished.
		"the field alone takes every value": {[]RequestParameter{ParameterToolChoice, ParameterTools}, all},
		"a value takes its field":           {[]RequestParameter{ParameterToolChoiceNone, ParameterTools}, []RequestParameter{ParameterToolChoice, ParameterToolChoiceNone, ParameterTools}},
		"a stated value is exact":           {[]RequestParameter{ParameterToolChoice, ParameterToolChoiceNone}, []RequestParameter{ParameterToolChoice, ParameterToolChoiceNone}},
		"no tool choice stays none":         {[]RequestParameter{ParameterTools}, []RequestParameter{ParameterTools}},
	} {
		if got := CanonicalParameterSet(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	legacy := Route{AcceptedParameters: &[]RequestParameter{ParameterToolChoice, ParameterTools}}
	if _, refused := legacy.UnacceptedParameter(ParameterToolChoice, ParameterToolChoiceRequired, ParameterTools); refused {
		t.Error("a set naming the field alone refused a value")
	}
	stated := Route{AcceptedParameters: &[]RequestParameter{ParameterToolChoice, ParameterToolChoiceNone, ParameterTools}}
	if parameter, refused := stated.UnacceptedParameter(ParameterToolChoice, ParameterToolChoiceRequired, ParameterTools); !refused || parameter != ParameterToolChoiceRequired {
		t.Errorf("a stated set accepted a value it does not name: %q %v", parameter, refused)
	}
}

func TestIntersectingSetsNeverLeavesTheFieldToBeReadAsEveryValue(t *testing.T) {
	none := []RequestParameter{ParameterToolChoice, ParameterToolChoiceNone, ParameterTools}
	required := []RequestParameter{ParameterToolChoice, ParameterToolChoiceRequired, ParameterTools}
	got := IntersectParameterSets(&none, &required)
	if got == nil || !slices.Equal(*got, []RequestParameter{ParameterTools}) {
		t.Fatalf("none ∩ required = %v, want tools alone: the field with no value would read as every value", got)
	}
	legacy := []RequestParameter{ParameterToolChoice, ParameterTools}
	if got := IntersectParameterSets(&legacy, &none); got == nil || !slices.Equal(*got, none) {
		t.Errorf("field-only ∩ none = %v, want %v", got, none)
	}
	if got := IntersectParameterSets(nil, &legacy); got == nil || len(*got) != 5 {
		t.Errorf("an abstaining statement narrowed: %v", got)
	}
	if IntersectParameterSets(nil, nil) != nil {
		t.Error("two unknown statements became a known one")
	}
}
