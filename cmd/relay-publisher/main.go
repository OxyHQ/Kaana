// Command relay-publisher builds the deployment inventory and re-issues it.
//
// It is a SEPARATE process from `cmd/relay` on purpose. Writing the inventory
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

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/providerconfig"
	"github.com/OxyHQ/Relay/internal/publisher"
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

	attribution, err := publisher.LoadAttribution(environmentOr("RELAY_PUBLISHER_ATTRIBUTION_PATH", "/etc/relay/model-attribution.json"))
	if err != nil {
		return err
	}

	providers, skipped, err := parsePublishableProviders(os.Getenv)
	if err != nil {
		return err
	}
	for _, slug := range skipped {
		// Not fatal, and the whole reason it is a warning: a snapshot may not
		// name a provider whose key does not exist, because there is no value
		// of RELAY_PROVIDERS that serves it without either refusing its
		// references or pinning a permanent `unconfigured` alarm. So the
		// provider is left out and said out loud.
		logger.Warn("a declared provider holds no credential, so it is absent from the snapshot and no reference will route to it",
			"provider", slug, "variable", providerconfig.EnvironmentPrefix(slug)+"_API_KEY")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	store, err := publisher.NewS3Store(
		client,
		os.Getenv("RELAY_INVENTORY_BUCKET"),
		os.Getenv("RELAY_INVENTORY_KEY"),
		os.Getenv("AWS_REGION"),
		credentialSource(client),
	)
	if err != nil {
		return err
	}

	interval, err := intervalFromEnv("RELAY_PUBLISH_INTERVAL", publisher.DefaultInterval)
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

	return inventoryPublisher.Run(ctx)
}

// parsePublishableProviders resolves the providers to ask, and the declared
// ones that hold no credential.
//
// It reads the same `RELAY_PROVIDERS` list and the same `RELAY_PROVIDER_<SLUG>_*`
// variables as the serving process, through the same `providerconfig` helpers,
// so the two commands cannot disagree about where a provider lives.
func parsePublishableProviders(getenv func(string) string) ([]publisher.Provider, []contract.ProviderSlug, error) {
	declared := providerconfig.SplitList(getenv("RELAY_PROVIDERS"))
	if len(declared) == 0 {
		return nil, nil, errors.New("RELAY_PROVIDERS is required: it lists the provider slugs to ask, and an empty one would publish a snapshot naming nothing")
	}

	var (
		providers []publisher.Provider
		skipped   []contract.ProviderSlug
	)
	seenSlug := make(map[contract.ProviderSlug]struct{}, len(declared))
	seenPrefix := make(map[string]contract.ProviderSlug, len(declared))

	for _, name := range declared {
		slug := contract.ProviderSlug(name)
		if !slug.Valid() {
			return nil, nil, fmt.Errorf("RELAY_PROVIDERS names %q, which is not a provider slug", name)
		}
		if _, duplicate := seenSlug[slug]; duplicate {
			return nil, nil, fmt.Errorf("RELAY_PROVIDERS names %q twice", slug)
		}
		seenSlug[slug] = struct{}{}

		prefix := providerconfig.EnvironmentPrefix(slug)
		if other, collides := seenPrefix[prefix]; collides {
			return nil, nil, fmt.Errorf("providers %q and %q both read their configuration from %s_*", other, slug, prefix)
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
			return nil, nil, fmt.Errorf("%s_BASE_URL is required: this build knows no address for the provider slug %q", prefix, slug)
		}
		if protocol != providerconfig.ProtocolOpenAICompatible {
			// Discovery speaks one shape, `GET /models`, and only the
			// OpenAI-compatible providers answer it. Anthropic publishes no
			// such list; a hand-written list for it would be the checked-in
			// file this command exists to replace, so it is refused rather
			// than invented.
			return nil, nil, fmt.Errorf("provider %q speaks %s, which publishes no model list this command can read; remove it from RELAY_PROVIDERS for the publisher, or its models have to be declared by something that measured them", slug, protocol)
		}

		keys := providerconfig.SplitList(getenv(prefix + "_API_KEY"))
		if len(keys) == 0 {
			skipped = append(skipped, slug)
			continue
		}
		// One key, not the pool: listing models is a single unmetered call, and
		// the serving process owns the rotation.
		providers = append(providers, publisher.Provider{Slug: slug, BaseURL: baseURL, APIKey: keys[0]})
	}

	if len(providers) == 0 {
		return nil, nil, errors.New("no declared provider holds a credential, so every reference would be unservable and the snapshot would name nothing")
	}
	return providers, skipped, nil
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
// RELAY_PROVIDERS looks exactly like an attribution for a provider that serves
// nothing, and both produce a snapshot missing routes somebody expected.
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

func slugsOf(providers []publisher.Provider) []string {
	slugs := make([]string, 0, len(providers))
	for _, target := range providers {
		slugs = append(slugs, string(target.Slug))
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
