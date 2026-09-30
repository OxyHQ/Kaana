package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
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
//     re-derived from the UNION of its zero-retention endpoints' lists. The
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
}

type zeroRetentionListResponse struct {
	Data []json.RawMessage `json:"data"`
}

type zeroRetentionEndpoint struct {
	ModelID string `json:"model_id"`
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

	models := make(map[string]*zeroRetentionModel)
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
		model := models[id]
		if model == nil {
			model = &zeroRetentionModel{parameters: map[string]bool{}, complete: true}
			models[id] = model
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		parameters, ok := stringListField(fields, "supported_parameters")
		if !ok {
			model.complete = false
			continue
		}
		for _, parameter := range parameters {
			model.parameters[parameter] = true
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
		observed := model.Observed
		if observed == nil {
			observed = &inventory.Observed{}
		}
		if reachable.complete {
			parameters := make([]string, 0, len(reachable.parameters))
			for parameter := range reachable.parameters {
				parameters = append(parameters, parameter)
			}
			applySupportedParameters(observed, parameters)
		} else {
			// The all-endpoint `/models` statement is not about the reachable
			// endpoints, and the reachable statement is incomplete: say nothing.
			observed.SupportsTools = nil
			observed.ReasoningEfforts = nil
			observed.AcceptedParameters = nil
		}
		if encoded, err := json.Marshal(observed); err == nil && string(encoded) == "{}" {
			observed = nil
		}
		model.Observed = observed
	}
}
