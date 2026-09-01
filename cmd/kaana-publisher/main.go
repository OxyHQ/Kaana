// Command kaana-publisher builds the deployment inventory and re-issues it.
//
// It is a SEPARATE process from `cmd/kaana` on purpose. Writing the inventory
// decides which deployment every model reference resolves to, which is a much
// larger authority than serving one request — so the permission to write the
// object belongs to a task role that does nothing else, rather than to the role
// the serving process runs under. Nothing here serves traffic and nothing here
// reads a customer request.
//
// Everything comes from the environment, one attribution table, and the
// providers' own model endpoints. It writes exactly one object.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/publisher"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("the inventory publisher stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	attribution, err := publisher.LoadAttribution(environmentOr("KAANA_PUBLISHER_ATTRIBUTION_PATH", "/etc/kaana-publisher/model-attribution.json"))
	if err != nil {
		return err
	}

	providers, err := parsePublishableProviders(os.Getenv)
	if err != nil {
		return err
	}
	credentialContext, cancelCredentialLoad := context.WithTimeout(ctx, 45*time.Second)
	credentialStore, credentialDatabase, err := credentialstore.Open(
		credentialContext,
		strings.TrimSpace(os.Getenv("DATABASE_URL")),
		strings.TrimSpace(os.Getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN")),
	)
	if err != nil {
		cancelCredentialLoad()
		return err
	}
	declarations, _, err := credentialStore.Load(credentialContext, slugsOf(providers))
	cancelCredentialLoad()
	if err != nil {
		credentialDatabase.Close()
		return err
	}
	defer credentialDatabase.Close()
	if err := attachDiscoveryCredentials(providers, declarations); err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Second}
	store, err := publisher.NewS3Store(
		client,
		os.Getenv("KAANA_INVENTORY_BUCKET"),
		os.Getenv("KAANA_INVENTORY_KEY"),
		os.Getenv("AWS_REGION"),
		credentialSource(client),
	)
	if err != nil {
		return err
	}

	interval, err := intervalFromEnv("KAANA_PUBLISH_INTERVAL", publisher.DefaultInterval)
	if err != nil {
		return err
	}
	credentialReloadInterval, err := intervalFromEnv("KAANA_CREDENTIAL_RELOAD_INTERVAL", time.Minute)
	if err != nil {
		return err
	}

	inventoryPublisher, err := publisher.New(publisher.Config{
		Providers:   providers,
		Attribution: attribution,
		Store:       store,
		Interval:    interval,
		Client:      client,
		Logger:      logger,
	})
	if err != nil {
		return err
	}

	warnAboutUnaskedAttributions(logger, attribution, providers)

	logger.Info("publishing the deployment inventory",
		"destination", store.Describe(),
		"interval", inventoryPublisher.Interval(),
		"horizon", inventory.DefaultMaxSnapshotAge,
		"providers", slugsOf(providers))

	go reloadPublisherCredentials(
		ctx,
		credentialStore,
		providers,
		inventoryPublisher,
		logger,
		credentialReloadInterval,
	)

	return inventoryPublisher.Run(ctx)
}

func reloadPublisherCredentials(
	ctx context.Context,
	store *credentialstore.Store,
	providerConfigs []publisher.Provider,
	inventoryPublisher *publisher.Publisher,
	logger *slog.Logger,
	every time.Duration,
) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			loadContext, cancel := context.WithTimeout(ctx, 45*time.Second)
			declarations, _, err := store.Load(loadContext, slugsOf(providerConfigs))
			cancel()
			if err != nil {
				logger.Error("publisher credentials could not be reloaded; keeping the last complete generation", "error", err)
				continue
			}
			replacement := append([]publisher.Provider(nil), providerConfigs...)
			if err := attachDiscoveryCredentials(replacement, declarations); err != nil {
				logger.Error("publisher credential reload was incomplete; keeping the previous generation", "error", err)
				continue
			}
			if err := inventoryPublisher.ReplaceProviders(replacement); err != nil {
				logger.Error("publisher credential generation could not be replaced", "error", err)
				continue
			}
			logger.Info("publisher credentials reloaded from Kaana's database", "providers", len(replacement))
		}
	}
}

// parsePublishableProviders resolves the non-secret provider configuration.
//
// It reads `KAANA_DISCOVERY_PROVIDERS` and the same non-secret
// `KAANA_PROVIDER_<SLUG>_*` variables as the serving process, through the same
// `providerconfig` helpers, so the two commands cannot disagree about where a
// provider lives. KAANA_PROVIDERS is accepted as a compatibility fallback for
// existing publisher task definitions, but a dedicated discovery set is what
// lets serving-only providers coexist without making the publisher fail.
// Credentials are attached from PostgreSQL after this returns.
func parsePublishableProviders(getenv func(string) string) ([]publisher.Provider, error) {
	providerSetVariable := "KAANA_DISCOVERY_PROVIDERS"
	declared := providerconfig.SplitList(getenv(providerSetVariable))
	if len(declared) == 0 {
		providerSetVariable = "KAANA_PROVIDERS"
		declared = providerconfig.SplitList(getenv(providerSetVariable))
	}
	if len(declared) == 0 {
		return nil, errors.New("KAANA_DISCOVERY_PROVIDERS is required (KAANA_PROVIDERS is the compatibility fallback): an empty set would publish a snapshot naming nothing")
	}
	if providerSetVariable == "KAANA_DISCOVERY_PROVIDERS" {
		serving := providerconfig.SplitList(getenv("KAANA_PROVIDERS"))
		if len(serving) == 0 {
			return nil, errors.New("KAANA_PROVIDERS is required alongside KAANA_DISCOVERY_PROVIDERS: the publisher must prove every discovered provider is served")
		}
		servedAt := make(map[contract.ProviderSlug]int, len(serving))
		seenPrefix := make(map[string]contract.ProviderSlug, len(serving))
		for index, name := range serving {
			slug := contract.ProviderSlug(name)
			if !slug.Valid() {
				return nil, fmt.Errorf("KAANA_PROVIDERS names %q, which is not a provider slug", name)
			}
			if _, duplicate := servedAt[slug]; duplicate {
				return nil, fmt.Errorf("KAANA_PROVIDERS names %q twice", slug)
			}
			servedAt[slug] = index

			prefix := providerconfig.EnvironmentPrefix(slug)
			if other, collides := seenPrefix[prefix]; collides {
				return nil, fmt.Errorf("providers %q and %q both read their configuration from %s_*", other, slug, prefix)
			}
			seenPrefix[prefix] = slug
		}
		previousIndex := -1
		for _, name := range declared {
			index, ok := servedAt[contract.ProviderSlug(name)]
			if !ok {
				return nil, fmt.Errorf("KAANA_DISCOVERY_PROVIDERS names %q, which KAANA_PROVIDERS does not serve", name)
			}
			if index <= previousIndex {
				return nil, fmt.Errorf("KAANA_DISCOVERY_PROVIDERS reorders %q relative to KAANA_PROVIDERS; discovery must preserve serving priority", name)
			}
			previousIndex = index
		}
	}

	var providers []publisher.Provider
	seenSlug := make(map[contract.ProviderSlug]struct{}, len(declared))
	seenPrefix := make(map[string]contract.ProviderSlug, len(declared))

	for _, name := range declared {
		slug := contract.ProviderSlug(name)
		if !slug.Valid() {
			return nil, fmt.Errorf("%s names %q, which is not a provider slug", providerSetVariable, name)
		}
		if _, duplicate := seenSlug[slug]; duplicate {
			return nil, fmt.Errorf("%s names %q twice", providerSetVariable, slug)
		}
		seenSlug[slug] = struct{}{}

		prefix := providerconfig.EnvironmentPrefix(slug)
		if other, collides := seenPrefix[prefix]; collides {
			return nil, fmt.Errorf("providers %q and %q both read their configuration from %s_*", other, slug, prefix)
		}
		seenPrefix[prefix] = slug

		known := providerconfig.Known[slug]
		protocol := known.Protocol
		if declaredProtocol := strings.TrimSpace(getenv(prefix + "_PROTOCOL")); declaredProtocol != "" {
			protocol = declaredProtocol
		}
		baseURL := known.BaseURL
		if declaredBaseURL := strings.TrimSpace(getenv(prefix + "_BASE_URL")); declaredBaseURL != "" {
			baseURL = declaredBaseURL
		}
		if baseURL == "" {
			return nil, fmt.Errorf("%s_BASE_URL is required: this build knows no address for the provider slug %q", prefix, slug)
		}
		if err := providerconfig.ValidateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("%s_BASE_URL for provider %q: %w", prefix, slug, err)
		}
		if protocol != providerconfig.ProtocolOpenAICompatible {
			// Discovery speaks one shape, `GET /models`, and only the
			// OpenAI-compatible providers answer it. Anthropic publishes no
			// such list; a hand-written list for it would be the checked-in
			// file this command exists to replace, so it is refused rather
			// than invented.
			return nil, fmt.Errorf("provider %q speaks %s, which publishes no model list this command can read; remove it from %s, or its models have to be declared by something that measured them", slug, protocol, providerSetVariable)
		}
		if known.Discovery == providerconfig.DiscoveryNotAvailable {
			return nil, fmt.Errorf("provider %q has no documented account model-list endpoint, so the publisher cannot verify what this credential can serve", slug)
		}

		regionValue := strings.TrimSpace(getenv(prefix + "_REGIONS"))
		regionNames := providerconfig.SplitList(regionValue)
		if regionValue != "" && len(regionNames) == 0 {
			return nil, fmt.Errorf("%s_REGIONS declares no region", prefix)
		}
		var regions []contract.Region
		if len(regionNames) > 0 {
			regions = make([]contract.Region, 0, len(regionNames))
		}
		seenRegions := make(map[contract.Region]struct{}, len(regionNames))
		for _, name := range regionNames {
			region := contract.Region(name)
			if !region.Valid() {
				return nil, fmt.Errorf("%s_REGIONS names %q, which is not an inference region", prefix, name)
			}
			if _, duplicate := seenRegions[region]; duplicate {
				return nil, fmt.Errorf("%s_REGIONS names %q twice", prefix, region)
			}
			seenRegions[region] = struct{}{}
			regions = append(regions, region)
		}

		providers = append(providers, publisher.Provider{
			Slug: slug, BaseURL: baseURL, Regions: regions, Discovery: known.Discovery,
		})
	}
	return providers, nil
}

// attachDiscoveryCredentials selects one key because listing models is one
// unmetered call. Serving owns pool rotation and retirement.
func attachDiscoveryCredentials(providers []publisher.Provider, declarations map[contract.ProviderSlug][]provider.KeyDeclaration) error {
	for index := range providers {
		pool := declarations[providers[index].Slug]
		if len(pool) == 0 {
			return fmt.Errorf("provider %q has no credential loaded from Kaana's database", providers[index].Slug)
		}
		providers[index].APIKey = pool[0].Secret
	}
	return nil
}

// credentialSource prefers the ECS task role and falls back to the environment.
//
// The task role is the production lane; the static keys are how this runs
// against a bucket from a laptop. Neither is defaulted into existence: with
// neither present the signer refuses, naming what is missing.
func credentialSource(client *http.Client) publisher.CredentialSource {
	if relative := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); relative != "" {
		return publisher.ContainerCredentials(client, "http://169.254.170.2"+relative)
	}
	if full := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"); full != "" {
		return publisher.ContainerCredentials(client, full)
	}
	return publisher.StaticCredentials(
		os.Getenv("AWS_ACCESS_KEY_ID"),
		os.Getenv("AWS_SECRET_ACCESS_KEY"),
		os.Getenv("AWS_SESSION_TOKEN"),
	)
}

// warnAboutUnaskedAttributions names a table entry for a provider nobody asks.
//
// It is otherwise invisible: an attribution for a provider that is not in
// KAANA_DISCOVERY_PROVIDERS looks exactly like an attribution for a provider
// that serves nothing, and both produce a snapshot missing routes somebody
// expected.
func warnAboutUnaskedAttributions(logger *slog.Logger, attribution *publisher.Attribution, providers []publisher.Provider) {
	asked := make(map[contract.ProviderSlug]struct{}, len(providers))
	for _, target := range providers {
		asked[target.Slug] = struct{}{}
	}
	for _, slug := range attribution.Providers() {
		if _, present := asked[slug]; !present {
			logger.Warn("the attribution table declares models for a provider this run does not ask; none of them can reach the snapshot",
				"provider", slug)
		}
	}
}

func slugsOf(providers []publisher.Provider) []contract.ProviderSlug {
	slugs := make([]contract.ProviderSlug, 0, len(providers))
	for _, target := range providers {
		slugs = append(slugs, target.Slug)
	}
	return slugs
}

func environmentOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// intervalFromEnv reads the cadence, refusing a value it cannot parse rather
// than falling back to the default — a typo that silently became fifteen
// minutes would be indistinguishable from the operator's intent.
func intervalFromEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is %q, which is not a duration: %w", name, value, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s is %q; a cadence has to be positive", name, value)
	}
	return parsed, nil
}
