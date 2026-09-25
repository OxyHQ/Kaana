package publisher

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/providercost"
)

// observeModelListEntry keeps what one provider's `GET /models` entry says
// about a model, and nothing it does not say.
//
// It reads the fields the OpenAI-compatible model lists Kaana discovers from
// actually publish, under the spellings those providers use:
//
//	name                               OpenRouter, Mistral
//	created (unix seconds)             OpenAI, OpenRouter, Groq, Cerebras, xAI, ...
//	context_length                     OpenRouter
//	context_window                     Groq
//	max_context_length                 Mistral
//	top_provider.max_completion_tokens OpenRouter
//	max_completion_tokens              Groq
//	architecture.input_modalities      OpenRouter
//	architecture.output_modalities     OpenRouter
//	supported_parameters               OpenRouter ("tools", "reasoning", "reasoning_effort")
//	capabilities.function_calling      Mistral
//	pricing.prompt / pricing.completion OpenRouter, USD per token (only when
//	                                    publishesUSDPerTokenPrices)
//
// Every field is decoded independently and LENIENTLY: a field of an
// unexpected type, a zero, a negative or an empty value is dropped, never
// defaulted, and never fails discovery. A provider changing the type of a
// metadata field must not withdraw its models from the inventory; it may only
// make the catalogue say less about them.
//
// The returned value is nil when the entry said nothing Kaana could keep.
func observeModelListEntry(raw json.RawMessage, publishesUSDPerTokenPrices bool) *inventory.Observed {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	observed := inventory.Observed{}
	kept := false

	if name, ok := stringField(fields, "name"); ok && validDisplayName(name) {
		observed.DisplayName = &name
		kept = true
	}
	if created, ok := positiveIntField(fields, "created"); ok {
		stamp := contract.NewTimestamp(time.Unix(int64(created), 0))
		observed.CreatedAt = &stamp
		kept = true
	}
	for _, key := range []string{"context_length", "context_window", "max_context_length"} {
		if tokens, ok := positiveIntField(fields, key); ok {
			observed.ContextTokens = &tokens
			kept = true
			break
		}
	}
	if topProvider, ok := objectField(fields, "top_provider"); ok {
		if tokens, ok := positiveIntField(topProvider, "max_completion_tokens"); ok {
			observed.MaxOutputTokens = &tokens
			kept = true
		}
	}
	if observed.MaxOutputTokens == nil {
		if tokens, ok := positiveIntField(fields, "max_completion_tokens"); ok {
			observed.MaxOutputTokens = &tokens
			kept = true
		}
	}
	if architecture, ok := objectField(fields, "architecture"); ok {
		if modalities := modalityListField(architecture, "input_modalities"); modalities != nil {
			observed.InputModalities = modalities
			kept = true
		}
		if modalities := modalityListField(architecture, "output_modalities"); modalities != nil {
			observed.OutputModalities = modalities
			kept = true
		}
	}
	if parameters, ok := stringListField(fields, "supported_parameters"); ok {
		// A PRESENT list is a complete statement of what the model takes, so a
		// parameter it does not name is reported as unsupported. An absent list
		// says nothing, and both fields then stay absent.
		supported := make(map[string]bool, len(parameters))
		for _, parameter := range parameters {
			supported[parameter] = true
		}
		tools := supported["tools"]
		observed.SupportsTools = &tools
		efforts := []contract.ReasoningEffort{}
		if supported["reasoning"] || supported["reasoning_effort"] {
			// OpenRouter documents `reasoning.effort` as one normalized control
			// over every model that accepts `reasoning`, mapping it to the
			// upstream's own budget or effort. Its word "reasoning" is therefore
			// a statement about the whole effort vocabulary.
			efforts = contract.ReasoningEfforts()
		}
		observed.ReasoningEfforts = &efforts
		kept = true
	} else if capabilities, ok := objectField(fields, "capabilities"); ok {
		var functionCalling bool
		if value, present := capabilities["function_calling"]; present && json.Unmarshal(value, &functionCalling) == nil {
			observed.SupportsTools = &functionCalling
			kept = true
		}
	}
	if publishesUSDPerTokenPrices {
		if pricing, ok := objectField(fields, "pricing"); ok {
			prompt, promptOK := stringField(pricing, "prompt")
			completion, completionOK := stringField(pricing, "completion")
			if promptOK && completionOK {
				if price, err := providercost.ListPriceFromPerToken("USD", prompt, completion); err == nil {
					observed.ListPrice = &price
					kept = true
				}
			}
		}
	}

	if !kept {
		return nil
	}
	if observed.Validate() != nil {
		// Unreachable by construction; a metadata block the reader would refuse
		// must never cost the provider its routes, so it is dropped whole.
		return nil
	}
	return &observed
}

func stringField(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, present := fields[key]
	if !present {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func positiveIntField(fields map[string]json.RawMessage, key string) (int, bool) {
	raw, present := fields[key]
	if !present {
		return 0, false
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value <= 0 || value > 1<<31-1 {
		return 0, false
	}
	return int(value), true
}

func objectField(fields map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, present := fields[key]
	if !present {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func stringListField(fields map[string]json.RawMessage, key string) ([]string, bool) {
	raw, present := fields[key]
	if !present {
		return nil, false
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, false
	}
	return values, true
}

// modalityListField keeps a reported modality list only if every member is a
// modality token; a list with one unreadable member is not a list Kaana can
// narrow faithfully, so it is dropped whole rather than trimmed.
func modalityListField(fields map[string]json.RawMessage, key string) []string {
	values, ok := stringListField(fields, key)
	if !ok || len(values) == 0 {
		return nil
	}
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if !inventory.ValidModality(value) {
			return nil
		}
		set[value] = true
	}
	modalities := make([]string, 0, len(set))
	for value := range set {
		modalities = append(modalities, value)
	}
	sort.Strings(modalities)
	return modalities
}

func validDisplayName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name || !utf8.ValidString(name) || utf8.RuneCountInString(name) > inventory.MaxDisplayNameLength {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
