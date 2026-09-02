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
	// APIKey is ONE credential, not the pool. Listing models is one authenticated
	// catalogue question even when the provider paginates it, so rotating a
	// pool here would spend the operator's keys against a request whose failure
	// means "ask again later", not "this key is spent". The serving process
	// owns the pool; this asks a question.
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
// It selects a provider-owned discovery profile independently of the serving
// adapter. Most profiles speak an OpenAI-compatible `GET {base}/models` shape;
// Alibaba uses its authenticated native, paginated catalogue. A provider with
// no reviewed list contract is refused by the caller rather than guessed at —
// serving compatibility is never evidence that `/models` exists.
func Discover(ctx context.Context, client *http.Client, target Provider) ([]DiscoveredModel, error) {
	if target.APIKey == "" {
		// Unreachable through Publish, which filters keyless providers out
		// before it gets here, but a direct caller must not be able to publish
		// a provider it never authenticated against: the list would be
		// whatever an unauthenticated endpoint answers, which at several
		// providers is a public catalogue much larger than the account can
		// actually call.
		return nil, fmt.Errorf("publisher: %s has no credential, so its model list could not be read as this account", target.Slug)
	}
	if target.Discovery == providerconfig.DiscoveryAlibabaModels {
		return discoverAlibabaModels(ctx, client, target)
	}

	endpoint, err := discoveryEndpoint(target, 1)
	if err != nil {
		return nil, err
	}
	var list modelListResponse
	if err := readModelList(ctx, client, target, endpoint, "OpenAI-compatible", &list); err != nil {
		return nil, err
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

func readModelList(ctx context.Context, client *http.Client, target Provider, endpoint, shape string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("publisher: building the model list request for %s: %w", target.Slug, err)
	}
	request.Header.Set("Authorization", "Bearer "+target.APIKey)
	request.Header.Set("Accept", "application/json")

	response, err := provider.RefuseRedirects(client).Do(request)
	if err != nil {
		// The credential is in the request, and a transport error can quote the
		// request. Redact against the exact value rather than a pattern.
		return fmt.Errorf("publisher: asking %s for its model list: %s", target.Slug, provider.RedactSecret(err.Error(), target.APIKey))
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelListBytes))
	if err != nil {
		return fmt.Errorf("publisher: reading %s's model list: %s", target.Slug, provider.RedactSecret(err.Error(), target.APIKey))
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("publisher: %s answered %d to a model list request: %s",
			target.Slug, response.StatusCode, contract.SafeErrorText(provider.RedactSecret(string(body), target.APIKey)))
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("publisher: %s's model list is not the documented %s list shape: %w", target.Slug, shape, err)
	}
	return nil
}

func discoverAlibabaModels(ctx context.Context, client *http.Client, target Provider) ([]DiscoveredModel, error) {
	seen := make(map[string]struct{})
	models := make([]DiscoveredModel, 0)
	expectedTotal := -1
	observed := 0
	for page := 1; ; page++ {
		endpoint, err := discoveryEndpoint(target, page)
		if err != nil {
			return nil, err
		}
		var list alibabaModelListResponse
		if err := readModelList(ctx, client, target, endpoint, "Alibaba Model Studio", &list); err != nil {
			return nil, err
		}
		if !list.Success {
			return nil, fmt.Errorf("publisher: %s's model-list response reports success=false", target.Slug)
		}
		if list.Output.Total <= 0 || list.Output.Total > maxModelListEntries {
			return nil, fmt.Errorf("publisher: %s reports an invalid model-list total %d", target.Slug, list.Output.Total)
		}
		if expectedTotal == -1 {
			expectedTotal = list.Output.Total
		} else if list.Output.Total != expectedTotal {
			return nil, fmt.Errorf("publisher: %s's model-list total changed from %d to %d during pagination", target.Slug, expectedTotal, list.Output.Total)
		}
		if list.Output.PageNo != page || list.Output.PageSize != alibabaModelListPageSize || len(list.Output.Models) > list.Output.PageSize {
			return nil, fmt.Errorf("publisher: %s returned an inconsistent model-list page %d", target.Slug, page)
		}
		if len(list.Output.Models) == 0 {
			return nil, fmt.Errorf("publisher: %s returned an empty model-list page %d before the declared total", target.Slug, page)
		}

		for _, entry := range list.Output.Models {
			if entry.Model == "" || strings.TrimSpace(entry.Model) != entry.Model {
				return nil, fmt.Errorf("publisher: %s's model list contains a missing or whitespace-normalized model id", target.Slug)
			}
			if _, duplicate := seen[entry.Model]; duplicate {
				return nil, fmt.Errorf("publisher: %s's model list names %q twice", target.Slug, entry.Model)
			}
			seen[entry.Model] = struct{}{}
			observed++
			if len(entry.InferenceMetadata.ResponseModality) != 1 || entry.InferenceMetadata.ResponseModality[0] != "Text" {
				continue
			}
			models = append(models, DiscoveredModel{UpstreamModelID: entry.Model})
		}
		if observed > expectedTotal {
			return nil, fmt.Errorf("publisher: %s returned %d models after declaring a total of %d", target.Slug, observed, expectedTotal)
		}
		if observed == expectedTotal {
			break
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("publisher: %s reports serving no text-output inference models", target.Slug)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].UpstreamModelID < models[j].UpstreamModelID })
	return models, nil
}

// maxModelListBytes bounds a response this process will read into memory. A
// model list is kilobytes; anything approaching this is a misdirected endpoint.
const maxModelListBytes = 4 << 20

// The authenticated catalogues this publisher reads currently contain
// hundreds of rows. A five-figure total is a malformed or misdirected response,
// not a catalogue this process should retain and page through indefinitely.
const maxModelListEntries = 10_000

type modelListResponse struct {
	Data []struct {
		ID string `json:"id"`

		Capabilities struct {
			CompletionChat bool `json:"completion_chat"`
		} `json:"capabilities"`
	} `json:"data"`
}

type alibabaModelListResponse struct {
	Success bool `json:"success"`
	Output  struct {
		Total    int `json:"total"`
		PageNo   int `json:"page_no"`
		PageSize int `json:"page_size"`
		Models   []struct {
			Model             string `json:"model"`
			InferenceMetadata struct {
				ResponseModality []string `json:"response_modality"`
			} `json:"inference_metadata"`
		} `json:"models"`
	} `json:"output"`
}

func discoveryEndpoint(target Provider, page int) (string, error) {
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
	case providerconfig.DiscoveryAlibabaModels:
		parsed, err := url.Parse(base)
		if err != nil || parsed.Path != "/compatible-mode/v1" {
			return "", fmt.Errorf("publisher: provider %s must use a compatibility base ending exactly in /compatible-mode/v1", target.Slug)
		}
		host := strings.ToLower(parsed.Hostname())
		switch {
		case strings.HasSuffix(host, ".ap-southeast-1.maas.aliyuncs.com"):
			parsed.Host = "dashscope-intl.aliyuncs.com"
		case strings.HasSuffix(host, ".cn-hongkong.maas.aliyuncs.com"):
			parsed.Host = "cn-hongkong.dashscope.aliyuncs.com"
		case host == "dashscope.aliyuncs.com", host == "dashscope-us.aliyuncs.com":
			return "", fmt.Errorf("publisher: provider %s's compatibility base does not identify the documented workspace-scoped catalogue origin; use the workspace-specific regional base for discovery", target.Slug)
		}
		parsed.Path = "/api/v1/models"
		query := parsed.Query()
		query.Add("capabilities", "TG")
		query.Add("supports", "inference")
		query.Set("page_no", fmt.Sprint(page))
		query.Set("page_size", fmt.Sprint(alibabaModelListPageSize))
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	case providerconfig.DiscoveryNotAvailable:
		return "", fmt.Errorf("publisher: provider %s publishes no account model list Kaana can verify", target.Slug)
	default:
		return "", fmt.Errorf("publisher: provider %s declares unknown discovery profile %q", target.Slug, target.Discovery)
	}
}

const alibabaModelListPageSize = 100
