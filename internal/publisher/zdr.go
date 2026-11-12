package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Every OpenRouter request Kaana sends carries the fixed policy
// `provider: {zdr: true, data_collection: "deny", require_parameters: true}`
// (openaicompat). OpenRouter therefore serves a Kaana request ONLY from that
// model's zero-data-retention endpoints, and only from one of those that
// accepts every parameter the request carries. What the model's `/models`
// entry says it takes is the union over ALL of its endpoints, most of which
// Kaana can never reach — so for OpenRouter that entry is the wrong source for
// what a Kaana route accepts.
//
// OpenRouter publishes the reachable set directly: `GET /api/v1/endpoints/zdr`
// lists every zero-data-retention endpoint with its `model_id` (the same id as
// `/models`) and its own `supported_parameters`. The publisher reads it once
// per cycle and, per model:
//
//   - a model with NO zero-retention endpoint can never be served under the
//     policy, so it is marked unservable and BuildSnapshot drops it with a
//     warning naming it;
//   - otherwise supportsTools, reasoningEfforts and acceptedParameters are
//     re-derived from the UNION of its zero-retention endpoints' lists, and
//     the tool-choice values from the union of their `supports_tool_choice`
//     (an endpoint listing `tool_choice` without one takes every value). The
//     union is the widest honest statement: a parameter no reachable endpoint
//     lists is certainly refused. It is not a guarantee that one endpoint takes
//     a whole COMBINATION; that residue is OpenRouter's 404, classified as a
//     request-level refusal by the adapter.
//
// The list is public and unauthenticated, so no credential is sent to it. A
// failed or malformed read fails OpenRouter's discovery for the cycle exactly
// as a failed `/models` read does: publishing without it would publish routes
// that cannot be served and parameter statements nobody made.
const zeroRetentionEndpointsPath = "/endpoints/zdr"

// maxZeroRetentionListBytes bounds the read. The list was 0.9 MiB on
// 2026-09-30 (921 endpoints over 326 models); a response near this bound is a
// misdirected endpoint, not a catalogue.
const maxZeroRetentionListBytes = 16 << 20

// zeroRetentionModel is what OpenRouter's zero-retention list says about one
// model.
type zeroRetentionModel struct {
	// parameters is the union of the endpoints' `supported_parameters`.
	parameters map[string]bool
	// complete is false when any endpoint's list was absent or unreadable: a
	// union missing one endpoint's words is not a complete statement, so the
	// derived fields stay absent rather than under-report.
	complete bool
	// toolChoiceValues is the union of the tool-choice values the endpoints
	// that list `tool_choice` take: their own `supports_tool_choice`
	// statement, or every value for an endpoint that publishes none.
	toolChoiceValues map[provider.RequestParameter]bool
	// upstreams are the OpenRouter provider slugs serving the model's
	// zero-retention endpoints: the part of each endpoint's `tag` before any
	// `/` (`deepinfra/us` is `deepinfra`), the spelling `provider.order` takes.
	upstreams map[string]bool
	// quantizations are the quantization levels the endpoints are published at
	// (`quantization`), the vocabulary the public `provider.quantizations`
	// filter speaks.
	quantizations map[string]bool
	// avoidUpstreams are the OpenRouter upstream slugs whose endpoints for this
	// model are all Safety Retention while another upstream is plain zero
	// retention: the request names them in `provider.ignore`.
	avoidUpstreams []string
}

// supportsToolChoiceValues maps OpenRouter's per-endpoint
// `supports_tool_choice` object onto Kaana's value words. `auto` needs no
// word (provider.ToolChoiceParameters).
var supportsToolChoiceValues = map[string]provider.RequestParameter{
	"none":     provider.ParameterToolChoiceNone,
	"required": provider.ParameterToolChoiceRequired,
	"function": provider.ParameterToolChoiceFunction,
}

// endpointToolChoiceValues reads one endpoint's `supports_tool_choice`:
// `{"none":bool,"auto":bool,"required":bool,"function":bool}`. A statement
// with any of the three values missing or not a boolean is no statement, and
// the endpoint's `tool_choice` then speaks for every value.
func endpointToolChoiceValues(fields map[string]json.RawMessage) map[provider.RequestParameter]bool {
	values := make(map[provider.RequestParameter]bool, len(supportsToolChoiceValues))
	statement, ok := objectField(fields, "supports_tool_choice")
	if ok {
		for word, parameter := range supportsToolChoiceValues {
			var supported *bool
			raw, present := statement[word]
			if !present || json.Unmarshal(raw, &supported) != nil || supported == nil {
				ok = false
				break
			}
			values[parameter] = *supported
		}
	}
	if !ok {
		for _, parameter := range provider.ToolChoiceValues() {
			values[parameter] = true
		}
	}
	return values
}

type zeroRetentionListResponse struct {
	Data []json.RawMessage `json:"data"`
}

type zeroRetentionEndpoint struct {
	ModelID      string `json:"model_id"`
	Tag          string `json:"tag"`
	Quantization string `json:"quantization"`
	// SafetyRetention marks an endpoint that keeps prompts to detect abuse. It
	// is in the zero-retention list but is not zero retention.
	SafetyRetention bool `json:"safety_retention"`
}

// readZeroRetentionEndpoints fetches and indexes OpenRouter's zero-retention
// endpoint list by model id.
func readZeroRetentionEndpoints(ctx context.Context, client *http.Client, target Provider) (map[string]*zeroRetentionModel, error) {
	endpoint := strings.TrimSuffix(target.BaseURL, "/") + zeroRetentionEndpointsPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("publisher: building the zero-retention endpoint request for %s: %w", target.Slug, err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := provider.RefuseRedirects(client).Do(request)
	if err != nil {
		// No credential is on this request; redacting the discovery key anyway
		// keeps every publisher error on one rule.
		return nil, fmt.Errorf("publisher: asking %s for its zero-retention endpoints: %s", target.Slug, provider.RedactSecret(err.Error(), target.APIKey))
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxZeroRetentionListBytes))
	if err != nil {
		return nil, fmt.Errorf("publisher: reading %s's zero-retention endpoints: %s", target.Slug, provider.RedactSecret(err.Error(), target.APIKey))
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("publisher: %s answered %d to a zero-retention endpoint request: %s",
			target.Slug, response.StatusCode, contract.SafeErrorText(provider.RedactSecret(string(body), target.APIKey)))
	}
	var list zeroRetentionListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("publisher: %s's zero-retention endpoint list is not the documented shape: %w", target.Slug, err)
	}
	if len(list.Data) == 0 {
		// Every model would be dropped as unservable. An empty list is far
		// likelier a broken answer than OpenRouter withdrawing every
		// zero-retention endpoint at once, and withdrawing OpenRouter's routes
		// on it is what a failed read already does, loudly.
		return nil, fmt.Errorf("publisher: %s reports no zero-retention endpoints at all", target.Slug)
	}

	// First pass: group the endpoints by model, so a model's Safety Retention
	// endpoints can be told apart from its zero-retention ones before anything
	// is derived from them.
	type listed struct {
		raw      json.RawMessage
		entry    zeroRetentionEndpoint
		upstream string
	}
	byModel := make(map[string][]listed)
	for _, raw := range list.Data {
		var entry zeroRetentionEndpoint
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("publisher: %s's zero-retention endpoint list is not the documented shape: %w", target.Slug, err)
		}
		id := strings.TrimSpace(entry.ModelID)
		if id == "" {
			// The model id is the identity this list is joined on; an entry
			// without one cannot be attributed to any model, so the list is not
			// one Kaana can read faithfully.
			return nil, fmt.Errorf("publisher: %s's zero-retention endpoint list contains an endpoint with no model_id", target.Slug)
		}
		upstream, _, _ := strings.Cut(strings.TrimSpace(entry.Tag), "/")
		byModel[id] = append(byModel[id], listed{raw: raw, entry: entry, upstream: upstream})
	}

	models := make(map[string]*zeroRetentionModel, len(byModel))
	for id, endpoints := range byModel {
		model := &zeroRetentionModel{parameters: map[string]bool{}, complete: true, toolChoiceValues: map[provider.RequestParameter]bool{}, upstreams: map[string]bool{}, quantizations: map[string]bool{}}
		models[id] = model

		// A Safety Retention endpoint keeps prompts to detect abuse, which is not
		// zero retention. OpenRouter's `zdr: true` still admits it unless the
		// account is set to ZDR only, so when the model ALSO has a plain
		// zero-retention endpoint, the Safety Retention upstreams are named in the
		// request's `provider.ignore` (avoidUpstreams) and their endpoints say
		// nothing about what Kaana can reach. A model that has only Safety
		// Retention endpoints has nothing to fall back to and is kept as it was:
		// docs/inventory.md records the dependency.
		plain := map[string]bool{}
		for _, endpoint := range endpoints {
			if !endpoint.entry.SafetyRetention {
				plain[endpoint.upstream] = true
			}
		}
		avoid := map[string]bool{}
		for _, endpoint := range endpoints {
			if endpoint.entry.SafetyRetention && len(plain) > 0 && endpoint.upstream != "" && !plain[endpoint.upstream] {
				avoid[endpoint.upstream] = true
			}
		}
		for upstream := range avoid {
			model.avoidUpstreams = append(model.avoidUpstreams, upstream)
		}
		slices.Sort(model.avoidUpstreams)

		for _, endpoint := range endpoints {
			if endpoint.entry.SafetyRetention && len(plain) > 0 {
				continue
			}
			if endpoint.upstream != "" {
				model.upstreams[endpoint.upstream] = true
			}
			if word := strings.TrimSpace(endpoint.entry.Quantization); word != "" {
				if !inventory.ValidQuantization(word) {
					// A word outside the vocabulary is "unknown", never a guess
					// at a level. An absent field says nothing and records nothing.
					word = "unknown"
				}
				model.quantizations[word] = true
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(endpoint.raw, &fields)
			parameters, ok := stringListField(fields, "supported_parameters")
			if !ok {
				model.complete = false
				continue
			}
			for _, parameter := range parameters {
				model.parameters[parameter] = true
				if parameter == "tool_choice" {
					for value, supported := range endpointToolChoiceValues(fields) {
						model.toolChoiceValues[value] = model.toolChoiceValues[value] || supported
					}
				}
			}
		}
	}
	return models, nil
}

// restrictToZeroRetention applies the zero-retention list to OpenRouter's
// discovered models. See zeroRetentionEndpointsPath.
func restrictToZeroRetention(models []DiscoveredModel, zeroRetention map[string]*zeroRetentionModel) {
	for index := range models {
		model := &models[index]
		reachable, ok := zeroRetention[model.UpstreamModelID]
		if !ok {
			model.Unservable = "OpenRouter lists no zero-data-retention endpoint for it, and every Kaana request to OpenRouter requires one"
			continue
		}
		if order := providerconfig.OpenRouterUpstreamOrder(model.UpstreamModelID); len(order) > 0 &&
			!slices.ContainsFunc(order, func(upstream string) bool { return reachable.upstreams[upstream] }) {
			// Its requests name only these upstreams and allow no fallback, so
			// with none of them zero-retention it can never be served.
			model.Unservable = "none of its pinned OpenRouter upstreams (providerconfig.OpenRouterUpstreamOrder) has a zero-data-retention endpoint for it"
			continue
		}
		observed := model.Observed
		if observed == nil {
			observed = &inventory.Observed{}
		}
		if reachable.complete {
			parameters := make([]string, 0, len(reachable.parameters))
			for parameter := range reachable.parameters {
				parameters = append(parameters, parameter)
			}
			applySupportedParameters(observed, parameters, reachable.toolChoiceValues)
		} else {
			// The all-endpoint `/models` statement is not about the reachable
			// endpoints, and the reachable statement is incomplete: say nothing.
			observed.SupportsTools = nil
			observed.ReasoningEfforts = nil
			observed.AcceptedParameters = nil
		}
		levels := make([]string, 0, len(reachable.quantizations))
		for level := range reachable.quantizations {
			levels = append(levels, level)
		}
		slices.Sort(levels)
		if len(levels) > 0 {
			observed.Quantizations = levels
		}
		if len(reachable.avoidUpstreams) > 0 {
			observed.AvoidUpstreams = reachable.avoidUpstreams
		}
		if encoded, err := json.Marshal(observed); err == nil && string(encoded) == "{}" {
			observed = nil
		}
		model.Observed = observed
	}
}
