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
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/anthropic"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
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
//	supported_parameters               OpenRouter (tools, reasoning and every control in
//	                                   providerParameterWords; for OpenRouter the publisher
//	                                   replaces it with the zero-retention union, zdr.go)
//	capabilities.function_calling      Mistral
//	pricing.prompt / pricing.completion OpenRouter, USD per token (only when
//	                                    publishesUSDPerTokenPrices)
//
// Anthropic's native Models API uses other spellings and has its own reader,
// observeAnthropicModel.
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
		applySupportedParameters(&observed, parameters, nil)
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

// observeAnthropicModel keeps what one row of Anthropic's Models API
// (https://platform.claude.com/docs/en/api/models/list) says about a model:
//
//	display_name                      displayName
//	created_at (RFC 3339)             createdAt; Anthropic documents an epoch
//	                                  value for an unknown release date, which
//	                                  is absent, not 1970
//	max_input_tokens                  contextTokens
//	max_tokens                        maxOutputTokens (the `max_tokens` ceiling)
//	capabilities.effort               reasoningEfforts: the contract's low,
//	                                  medium and high that Anthropic marks
//	                                  supported, `[]` when effort itself is
//	                                  unsupported. It is the same
//	                                  `output_config.effort` the Messages
//	                                  adapter sends, so the list states exactly
//	                                  what the route will accept.
//	capabilities.image_input,         inputModalities: `text`, plus `image`
//	capabilities.pdf_input            and `file` (the catalogue's word for a
//	                                  document input, as OpenRouter publishes
//	                                  it for the same lines) where Anthropic
//	                                  marks them supported; outputModalities
//	                                  `text`. The two booleans state what a
//	                                  Messages request may carry BESIDES the
//	                                  text every Messages model takes and
//	                                  returns, so both must be readable; one
//	                                  that is not drops both lists rather than
//	                                  under-report.
//
// Nothing else is inferred from the row. Anthropic's capability tree names no
// tool-use or sampling words, so supportsTools and acceptedParameters come
// from the adapter's own per-model statement instead
// (applyStatedAcceptedParameters). The Messages list publishes no price.
//
// Every field is decoded independently and leniently, as for
// observeModelListEntry: an unreadable value is dropped, never defaulted, and
// never fails discovery.
func observeAnthropicModel(raw json.RawMessage) *inventory.Observed {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	observed := inventory.Observed{}
	kept := false

	if name, ok := stringField(fields, "display_name"); ok && validDisplayName(name) {
		observed.DisplayName = &name
		kept = true
	}
	if created, ok := stringField(fields, "created_at"); ok {
		if at, err := time.Parse(time.RFC3339, created); err == nil && at.Unix() > 0 {
			stamp := contract.NewTimestamp(at)
			observed.CreatedAt = &stamp
			kept = true
		}
	}
	if tokens, ok := positiveIntField(fields, "max_input_tokens"); ok {
		observed.ContextTokens = &tokens
		kept = true
	}
	if tokens, ok := positiveIntField(fields, "max_tokens"); ok {
		observed.MaxOutputTokens = &tokens
		kept = true
	}
	if capabilities, ok := objectField(fields, "capabilities"); ok {
		if efforts, ok := anthropicEfforts(capabilities); ok {
			observed.ReasoningEfforts = &efforts
			kept = true
		}
		if inputs, ok := anthropicInputModalities(capabilities); ok {
			observed.InputModalities = inputs
			observed.OutputModalities = []string{"text"}
			kept = true
		}
	}

	if !kept {
		return nil
	}
	if observed.Validate() != nil {
		return nil
	}
	return &observed
}

// anthropicEfforts reads `capabilities.effort`: `{"supported": bool, "low":
// {"supported": bool}, "medium": ..., "high": ..., ...}`. A statement is kept
// only when it is complete for the contract's vocabulary; a level that is
// missing or unreadable drops the whole field rather than under-report it.
// Levels the contract does not name (`xhigh`, `max`) are not Kaana's to send.
func anthropicEfforts(capabilities map[string]json.RawMessage) ([]contract.ReasoningEffort, bool) {
	effort, ok := objectField(capabilities, "effort")
	if !ok {
		return nil, false
	}
	supported, ok := supportedFlag(effort)
	if !ok {
		return nil, false
	}
	efforts := []contract.ReasoningEffort{}
	if !supported {
		return efforts, true
	}
	for _, level := range contract.ReasoningEfforts() {
		statement, ok := objectField(effort, string(level))
		if !ok {
			return nil, false
		}
		levelSupported, ok := supportedFlag(statement)
		if !ok {
			return nil, false
		}
		if levelSupported {
			efforts = append(efforts, level)
		}
	}
	return efforts, true
}

// anthropicInputModalities reads `capabilities.image_input` and
// `capabilities.pdf_input` (each `{"supported": bool}`) into a sorted input
// modality list that always holds `text`. Either statement missing or
// unreadable drops the list: it would otherwise claim the model takes text
// alone.
func anthropicInputModalities(capabilities map[string]json.RawMessage) ([]string, bool) {
	inputs := []string{}
	for _, capability := range []struct{ key, modality string }{
		{"pdf_input", "file"},
		{"image_input", "image"},
	} {
		statement, ok := objectField(capabilities, capability.key)
		if !ok {
			return nil, false
		}
		supported, ok := supportedFlag(statement)
		if !ok {
			return nil, false
		}
		if supported {
			inputs = append(inputs, capability.modality)
		}
	}
	return append(inputs, "text"), true
}

// supportedFlag reads Anthropic's `{"supported": bool}` leaf.
func supportedFlag(fields map[string]json.RawMessage) (bool, bool) {
	raw, present := fields["supported"]
	if !present {
		return false, false
	}
	var value *bool
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false, false
	}
	return *value, true
}

// applySupportedParameters records what a PRESENT `supported_parameters`
// list says. A present list is a complete statement of what the model takes,
// so a parameter it does not name is reported as unsupported; an absent list
// says nothing and never reaches here, leaving every derived field absent.
//
// It sets the three fields that list decides — supportsTools,
// reasoningEfforts and acceptedParameters — together, so they can never be
// read from two different lists.
//
// `tool_choice` names the field, not its values. toolChoiceValues is the
// provider's own per-value statement where it publishes one (OpenRouter's
// `supports_tool_choice`, zdr.go); nil means it published none, and the field
// then speaks for every value. A statement that leaves no value drops the
// field too: a route that takes no value has nothing to send in it.
func applySupportedParameters(observed *inventory.Observed, parameters []string, toolChoiceValues map[provider.RequestParameter]bool) {
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

	stated := make(map[provider.RequestParameter]bool, len(providerParameterWords))
	for _, parameter := range provider.RequestParameters() {
		for _, word := range providerParameterWords[parameter] {
			if supported[word] {
				stated[parameter] = true
				break
			}
		}
	}
	if toolChoiceValues != nil && stated[provider.ParameterToolChoice] {
		anyValue := false
		for _, value := range provider.ToolChoiceValues() {
			stated[value] = toolChoiceValues[value]
			anyValue = anyValue || toolChoiceValues[value]
		}
		stated[provider.ParameterToolChoice] = anyValue
	}
	accepted := make([]provider.RequestParameter, 0, len(stated))
	for _, parameter := range provider.RequestParameters() {
		if stated[parameter] {
			accepted = append(accepted, parameter)
		}
	}
	observed.AcceptedParameters = &accepted
}

// applyStatedReasoningEfforts replaces reasoningEfforts with the adapter's own
// per-model statement wherever providerconfig.ReasoningEfforts holds one.
//
// That statement is what Translate enforces for the route, so publishing
// anything else would advertise an effort the route refuses. It matters for
// the catalogue because efforts are INTERSECTED across a line's deployments: an
// xAI deployment whose model list says nothing abstains, and the line then
// advertised OpenRouter's whole vocabulary to requests Oxy may sign onto the
// xAI route. A stated empty list is published as `[]` — "this route takes no
// effort" — which narrows the line to what every route accepts.
func applyStatedReasoningEfforts(slug contract.ProviderSlug, models []DiscoveredModel) {
	for index := range models {
		model := &models[index]
		efforts, stated := providerconfig.ReasoningEfforts(slug, model.UpstreamModelID)
		if !stated {
			continue
		}
		if model.Observed == nil {
			model.Observed = &inventory.Observed{}
		}
		model.Observed.ReasoningEfforts = &efforts
	}
}

// applyStatedAcceptedParameters publishes the Anthropic Messages adapter's own
// per-model statement of the controls a model takes
// (anthropic.AcceptedParameters) as the deployment's acceptedParameters, and
// supportsTools as whether it takes `tools`. Anthropic's Models API carries
// neither, and that statement is exactly what Translate enforces, so the
// catalogue cannot advertise a control the route refuses.
//
// `reasoning.effort` is narrowed by what Anthropic DID publish: a model whose
// `capabilities.effort` is unsupported (reasoningEfforts `[]`) does not take
// the control. A model nobody reviewed keeps both fields absent.
func applyStatedAcceptedParameters(slug contract.ProviderSlug, models []DiscoveredModel) {
	if slug != anthropic.Slug {
		return
	}
	for index := range models {
		model := &models[index]
		stated, ok := anthropic.AcceptedParameters(model.UpstreamModelID)
		if !ok {
			continue
		}
		if model.Observed == nil {
			model.Observed = &inventory.Observed{}
		}
		takesEffort := model.Observed.ReasoningEfforts == nil || len(*model.Observed.ReasoningEfforts) > 0
		accepted := make([]provider.RequestParameter, 0, len(stated))
		tools := false
		for _, parameter := range stated {
			if parameter == provider.ParameterReasoningEffort && !takesEffort {
				continue
			}
			tools = tools || parameter == provider.ParameterTools
			accepted = append(accepted, parameter)
		}
		model.Observed.AcceptedParameters = &accepted
		model.Observed.SupportsTools = &tools
	}
}

// providerParameterWords maps each request control onto the words an
// OpenAI-compatible `supported_parameters` list uses for it. A control is
// accepted when ANY of its words is listed:
//
//   - `max_tokens` and `max_completion_tokens` are one control. OpenRouter
//     maps either onto the upstream's own spelling (gpt-4o-mini's zero-retention
//     endpoints list only `max_completion_tokens` and serve `max_tokens`), and
//     Kaana sends `max_tokens`.
//   - `reasoning` and `reasoning_effort` are one control, as for
//     reasoningEfforts above.
//   - `response_format` is what Kaana sends for every non-text response
//     format; `structured_outputs` alone is not read as accepting it.
//   - `tool_choice` is the field and, unless the provider states its values
//     one by one (applySupportedParameters), every value it can carry.
//
// Every vocabulary member has an entry; `TestEveryRequestParameterHasAProviderWord`
// keeps a member added to the vocabulary from silently never being accepted.
var providerParameterWords = map[provider.RequestParameter][]string{
	provider.ParameterMaxOutputTokens:    {"max_tokens", "max_completion_tokens"},
	provider.ParameterReasoningEffort:    {"reasoning", "reasoning_effort"},
	provider.ParameterResponseFormat:     {"response_format"},
	provider.ParameterFrequencyPenalty:   {"frequency_penalty"},
	provider.ParameterPresencePenalty:    {"presence_penalty"},
	provider.ParameterSeed:               {"seed"},
	provider.ParameterStopSequences:      {"stop"},
	provider.ParameterTemperature:        {"temperature"},
	provider.ParameterTopP:               {"top_p"},
	provider.ParameterToolChoice:         {"tool_choice"},
	provider.ParameterToolChoiceFunction: {"tool_choice"},
	provider.ParameterToolChoiceNone:     {"tool_choice"},
	provider.ParameterToolChoiceRequired: {"tool_choice"},
	provider.ParameterTools:              {"tools"},
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
