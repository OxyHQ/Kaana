// Command relay runs the inference data plane.
//
// Everything it needs comes from the environment and one inventory file. No
// secret is read from this repository, and the only secret it holds at all is
// the upstream provider credential — the Oxy edge's key is a PUBLIC key, so
// Relay cannot construct an envelope it would itself accept.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/edgeauth"
	"github.com/OxyHQ/Relay/internal/httpapi"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/provider/anthropic"
	"github.com/OxyHQ/Relay/internal/provider/openaicompat"
	"github.com/OxyHQ/Relay/internal/providercost"
	"github.com/OxyHQ/Relay/internal/relay"
	"github.com/OxyHQ/Relay/internal/rotation"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("relay could not start", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	inventoryPath := os.Getenv("RELAY_INVENTORY_PATH")
	if inventoryPath == "" {
		return errors.New("RELAY_INVENTORY_PATH is required: without a deployment inventory nothing can be routed")
	}
	inventoryStore, err := inventory.NewStore(inventory.Config{
		Path:           inventoryPath,
		MaxSnapshotAge: durationFromEnv("RELAY_INVENTORY_MAX_AGE", inventory.DefaultMaxSnapshotAge),
		Logger:         logger,
	})
	if err != nil {
		return err
	}

	// Upstream rate cards are optional and hold no customer-facing amount. An
	// absent file means provider cost is not measured, which every measurement
	// then says rather than reporting zero.
	var costs *providercost.Cards
	if ratesPath := os.Getenv("RELAY_PROVIDER_RATES_PATH"); ratesPath != "" {
		costs, err = providercost.Load(ratesPath)
		if err != nil {
			return err
		}
	}

	keys, err := edgeauth.ParsePublicKeys(os.Getenv("RELAY_EDGE_PUBLIC_KEYS"))
	if err != nil {
		return fmt.Errorf("RELAY_EDGE_PUBLIC_KEYS: %w", err)
	}
	verifier, err := edgeauth.NewVerifier(keys, durationFromEnv("RELAY_EDGE_MAX_SKEW", edgeauth.DefaultMaxSkew))
	if err != nil {
		return err
	}

	adapters, err := buildAdapters()
	if err != nil {
		return err
	}
	registry, err := provider.NewRegistry(adapters...)
	if err != nil {
		return err
	}

	// Refuse to start when the inventory routes somewhere this process cannot
	// reach. The alternative is discovering it on a customer request, as a
	// deployment_unavailable for a provider that was never loaded.
	for _, slug := range inventoryStore.Current().Providers() {
		if _, found := registry.Lookup(slug); !found {
			return fmt.Errorf("the inventory routes to provider %q, which this build has no adapter for", slug)
		}
	}

	rotationRegistry := rotation.NewRegistry(rotation.Policy{
		FailuresToOpen:   intFromEnv("RELAY_BREAKER_FAILURES_TO_OPEN", 0),
		Cooldown:         durationFromEnv("RELAY_BREAKER_COOLDOWN", 0),
		MaxCooldown:      durationFromEnv("RELAY_BREAKER_MAX_COOLDOWN", 0),
		SuccessesToClose: intFromEnv("RELAY_BREAKER_SUCCESSES_TO_CLOSE", 0),
	}, nil)

	failoverAck, err := failoverAcknowledgement(os.Getenv("RELAY_ASSUME_FAILOVER_AUTHORIZED"))
	if err != nil {
		return err
	}
	if failoverAck != "" {
		logger.Warn("same-model failover is enabled without a routing policy to authorise it",
			"acknowledgement", failoverAck,
			"meaning", "every caller of this process is asserted to have a routing policy permitting same-model deployment failover across every deployment in this inventory")
	}

	executor, err := relay.NewExecutor(relay.Config{
		Inventory:                inventoryStore,
		Providers:                registry,
		Rotation:                 rotationRegistry,
		Costs:                    costs,
		AssumeFailoverAuthorized: failoverAck != "",
	})
	if err != nil {
		return err
	}

	server, err := httpapi.New(httpapi.Config{
		Executor:         executor,
		Verifier:         verifier,
		Registry:         registry,
		Inventory:        inventoryStore,
		Rotation:         rotationRegistry,
		Logger:           logger,
		MaxEnvelopeBytes: int64FromEnv("RELAY_MAX_ENVELOPE_BYTES", httpapi.DefaultMaxEnvelopeBytes),
	})
	if err != nil {
		return err
	}

	address := os.Getenv("RELAY_ADDR")
	if address == "" {
		address = ":8080"
	}
	httpServer := &http.Server{
		Addr:    address,
		Handler: server.Handler(),
		// No WriteTimeout: a generation legitimately runs longer than any value
		// that would be safe here, and a write deadline would truncate the
		// stream mid-answer. The request context, cancelled on client
		// disconnect, is what bounds a request.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("relay is listening",
		"address", address,
		"contractVersion", contract.ContractVersion,
		"providers", providerSlugs(registry),
		"edgeKeyIds", verifier.KeyIDs(),
		"snapshotId", inventoryStore.Current().SnapshotID(),
		"costMeasured", costs != nil,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go reloadSnapshots(ctx, inventoryStore, rotationRegistry, registry, logger,
		durationFromEnv("RELAY_INVENTORY_RELOAD_INTERVAL", 30*time.Second))

	failed := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		logger.Info("relay is draining")
		// In-flight generations finish; nothing new is accepted. A shorter
		// drain would cut streams a customer is already being charged for.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// failoverAcknowledgement reads the operator's statement that same-model
// failover is safe here despite the envelope carrying no routing policy.
//
// It is deliberately awkward to set. The published routing policy has a
// customer-facing switch for this behaviour that Relay is not sent, so enabling
// failover without it overrides a control on the customer's behalf — defensible
// for a first-party canary where the operator IS the caller, and for nothing
// else. Requiring a reason and a date means the setting cannot arrive as an
// empty string, cannot be copied forward without someone reading it, and names
// whoever will be asked about it. An unparseable value refuses to start rather
// than falling back to either behaviour: both defaults would be wrong to
// choose silently.
func failoverAcknowledgement(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", nil
	}
	reason, date, found := strings.Cut(trimmed, ":")
	if !found || strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("RELAY_ASSUME_FAILOVER_AUTHORIZED must be `<reason>:<YYYY-MM-DD>`; it states who accepted serving failover without a routing policy, and when")
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(date)); err != nil {
		return "", fmt.Errorf("RELAY_ASSUME_FAILOVER_AUTHORIZED carries %q where a YYYY-MM-DD date belongs: %w", date, err)
	}
	return trimmed, nil
}

// reloadSnapshots re-reads the configuration snapshot on a fixed interval.
//
// A failed reload is not an outage and must not stop this loop: the case it
// exists for is a control plane that is failing repeatedly, and the store goes
// on serving the last good snapshot throughout. What degrades, and only after
// the staleness horizon, is the resolution of UNPINNED references — see
// inventory.Store.
func reloadSnapshots(
	ctx context.Context,
	store *inventory.Store,
	rotationRegistry *rotation.Registry,
	adapters *provider.Registry,
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
			if err := store.Reload(); err != nil {
				// Already logged by the store, together with the snapshot it is
				// still serving.
				continue
			}
			current := store.Current()
			for _, slug := range current.Providers() {
				if _, found := adapters.Lookup(slug); !found {
					// Not fatal: the rest of the snapshot is servable, and
					// killing a healthy process over one unroutable provider
					// would turn a partial configuration error into an outage.
					logger.Warn("the installed snapshot routes to a provider this build has no adapter for",
						"provider", slug, "snapshotId", current.SnapshotID())
				}
			}
			rotationRegistry.Retain(deploymentIDs(current))
		}
	}
}

func deploymentIDs(current *inventory.Inventory) []contract.DeploymentID {
	endpoints := current.Deployments()
	ids := make([]contract.DeploymentID, 0, len(endpoints))
	for _, endpoint := range endpoints {
		ids = append(ids, endpoint.DeploymentID)
	}
	return ids
}

// buildAdapters constructs every provider this build can serve.
//
// Two protocols are wired: OpenAI Chat Completions, which seven of the
// providers Alia speaks to share, and the Anthropic Messages API, which is one
// provider's own. Registering an adapter is not a claim that a credential
// exists for it — an unconfigured adapter reports itself `unconfigured` on the
// health surface rather than failing at the first request, which is what lets
// an operator see the gap before a customer does. What would be a claim this
// build cannot support is an INVENTORY entry routing to a provider with no
// credential, and the server refuses to start when the inventory names a
// provider it has no adapter for at all.
func buildAdapters() ([]provider.Adapter, error) {
	openaiBaseURL := os.Getenv("RELAY_PROVIDER_OPENAI_BASE_URL")
	if openaiBaseURL == "" {
		openaiBaseURL = "https://api.openai.com/v1"
	}
	openai, err := openaicompat.New(openaicompat.Config{
		Provider: "openai",
		BaseURL:  openaiBaseURL,
		APIKey:   os.Getenv("RELAY_PROVIDER_OPENAI_API_KEY"),
	})
	if err != nil {
		return nil, err
	}

	anthropicBaseURL := os.Getenv("RELAY_PROVIDER_ANTHROPIC_BASE_URL")
	if anthropicBaseURL == "" {
		anthropicBaseURL = "https://api.anthropic.com/v1"
	}
	claude, err := anthropic.New(anthropic.Config{
		BaseURL: anthropicBaseURL,
		APIKey:  os.Getenv("RELAY_PROVIDER_ANTHROPIC_API_KEY"),
	})
	if err != nil {
		return nil, err
	}

	return []provider.Adapter{openai, claude}, nil
}

func providerSlugs(registry *provider.Registry) []contract.ProviderSlug {
	slugs := make([]contract.ProviderSlug, 0)
	for _, adapter := range registry.All() {
		slugs = append(slugs, adapter.Provider())
	}
	return slugs
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func int64FromEnv(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func intFromEnv(name string, fallback int) int {
	return int(int64FromEnv(name, int64(fallback)))
}
