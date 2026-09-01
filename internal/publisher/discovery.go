package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Provider is one upstream the publisher asks what it serves.
//
// It carries a credential, which is why nothing in this package logs a Provider
// or embeds one in an error. The key is applied at send time and nowhere else,
// the same rule the adapters follow.
type Provider struct {
	Slug    contract.ProviderSlug
	BaseURL string
	// Regions are the upstream execution/residency regions every deployment
	// discovered through this API root may serve from. Provider model-list APIs
	// do not report them, so they must come from an explicit operator declaration
	// backed by the provider's own terms; AWS_REGION is unrelated.
	Regions []contract.Region
	// Discovery is the documented shape of this provider's model list.
	Discovery string
	// APIKey is ONE credential, not the pool. Listing models is a single
	// unmetered call, so rotating a pool here would spend the operator's keys
	// against a request whose failure means "ask again later", not "this key is
	// spent". The serving process owns the pool; this asks a question.
	APIKey string
}

// DiscoveredModel is one model a provider reports serving.
type DiscoveredModel struct {
	// UpstreamModelID is the id the provider's own API answers to. It is
	// carried through to the inventory verbatim: an id this package normalised
	// would be an id the provider 404s on.
	UpstreamModelID string
}

// Discover asks a provider which models it serves.
//
// It speaks the OpenAI-compatible `GET {base}/models` shape, which is what
// every provider this build carries an adapter for answers on. A provider whose
// protocol is not that one is not discoverable here and is refused by the
// caller rather than guessed at — Anthropic publishes no such list, and a
// hand-written list for it would be the checked-in file this whole command
// exists to replace.
func Discover(ctx context.Context, client *http.Client, target Provider) ([]DiscoveredModel, error) {
	if target.APIKey == "" {
		// Unreachable through Publish, which filters keyless providers out
		// before it gets here, but a direct caller must not be able to publish
		// a provider it never authenticated against: the list would be
		// whatever an unauthenticated endpoint answers, which at several
		// providers is a a public catalogue much larger than the account can
		// actually call.
		return nil, fmt.Errorf("publisher: %s has no credential, so its model list could not be read as this account", target.Slug)
	}

	endpoint, err := discoveryEndpoint(target)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("publisher: building the model list request for %s: %w", target.Slug, err)
	}
	request.Header.Set("Authorization", "Bearer "+target.APIKey)
	request.Header.Set("Accept", "application/json")

	response, err := provider.RefuseRedirects(client).Do(request)
	if err != nil {
		// The credential is in the request, and a transport error can quote the
		// request. Redact against the exact value rather than a pattern.
		return nil, fmt.Errorf("publisher: asking %s for its model list: %s", target.Slug, provider.RedactSecret(err.Error(), target.APIKey))
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelListBytes))
	if err != nil {
		return nil, fmt.Errorf("publisher: reading %s's model list: %s", target.Slug, provider.RedactSecret(err.Error(), target.APIKey))
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("publisher: %s answered %d to a model list request: %s",
			target.Slug, response.StatusCode, contract.SafeErrorText(provider.RedactSecret(string(body), target.APIKey)))
	}

	var list modelListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("publisher: %s's model list is not the OpenAI-compatible list shape: %w", target.Slug, err)
	}

	seen := make(map[string]struct{}, len(list.Data))
	models := make([]DiscoveredModel, 0, len(list.Data))
	for _, entry := range list.Data {
		if target.Discovery == providerconfig.DiscoveryMistralModels && !entry.Capabilities.CompletionChat {
			continue
		}
		if target.Discovery == providerconfig.DiscoveryNebiusModels && strings.HasSuffix(strings.TrimSpace(entry.ID), "-fast") {
			// Nebius documents `-fast` as an infrastructure flavour with
			// identical model outputs, not a second immutable model identity. A
			// provider-specific profile keeps it out before attribution can
			// accidentally bless one.
			continue
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			return nil, fmt.Errorf("publisher: %s's model list contains an entry with no id", target.Slug)
		}
		if _, duplicate := seen[id]; duplicate {
			// Two entries for one id would become two deployments of one
			// reference on one provider — a failover set whose members are the
			// same endpoint, which fails over to itself.
			return nil, fmt.Errorf("publisher: %s's model list names %q twice", target.Slug, id)
		}
		seen[id] = struct{}{}
		models = append(models, DiscoveredModel{UpstreamModelID: id})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("publisher: %s reports serving no models at all", target.Slug)
	}

	// Sorted so a snapshot's content — and therefore its id — does not change
	// because a provider reordered its list.
	sort.Slice(models, func(i, j int) bool { return models[i].UpstreamModelID < models[j].UpstreamModelID })
	return models, nil
}

// maxModelListBytes bounds a response this process will read into memory. A
// model list is kilobytes; anything approaching this is a misdirected endpoint.
const maxModelListBytes = 4 << 20

type modelListResponse struct {
	Data []struct {
		ID string `json:"id"`

		Capabilities struct {
			CompletionChat bool `json:"completion_chat"`
		} `json:"capabilities"`
	} `json:"data"`
}

func discoveryEndpoint(target Provider) (string, error) {
	base := strings.TrimSuffix(target.BaseURL, "/")
	switch target.Discovery {
	case "", providerconfig.DiscoveryOpenAIModels, providerconfig.DiscoveryMistralModels:
		return base + "/models", nil
	case providerconfig.DiscoveryNebiusModels:
		parsed, err := url.Parse(base + "/models")
		if err != nil {
			return "", fmt.Errorf("publisher: provider %s has an invalid model list address", target.Slug)
		}
		query := parsed.Query()
		query.Set("verbose", "true")
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	case providerconfig.DiscoverySiliconModels:
		parsed, err := url.Parse(base + "/models")
		if err != nil {
			return "", fmt.Errorf("publisher: provider %s has an invalid model list address", target.Slug)
		}
		query := parsed.Query()
		query.Set("type", "text")
		query.Set("sub_type", "chat")
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	case providerconfig.DiscoveryNotAvailable:
		return "", fmt.Errorf("publisher: provider %s publishes no account model list Kaana can verify", target.Slug)
	default:
		return "", fmt.Errorf("publisher: provider %s declares unknown discovery profile %q", target.Slug, target.Discovery)
	}
}
