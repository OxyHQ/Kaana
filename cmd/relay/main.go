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
	"syscall"
	"time"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/edgeauth"
	"github.com/OxyHQ/Relay/internal/httpapi"
	"github.com/OxyHQ/Relay/internal/inventory"
	"github.com/OxyHQ/Relay/internal/provider"
	"github.com/OxyHQ/Relay/internal/provider/openaicompat"
	"github.com/OxyHQ/Relay/internal/relay"
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
	inv, err := inventory.Load(inventoryPath)
	if err != nil {
		return err
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
	for _, slug := range inv.Providers() {
		if _, found := registry.Lookup(slug); !found {
			return fmt.Errorf("the inventory routes to provider %q, which this build has no adapter for", slug)
		}
	}

	server, err := httpapi.New(httpapi.Config{
		Executor:         relay.NewExecutor(inv, registry),
		Verifier:         verifier,
		Registry:         registry,
		Logger:           logger,
		MaxEnvelopeBytes: intFromEnv("RELAY_MAX_ENVELOPE_BYTES", httpapi.DefaultMaxEnvelopeBytes),
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
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

// buildAdapters constructs every provider this build can serve.
//
// One adapter is wired: `openai`, over the OpenAI Chat Completions protocol.
// The other providers Alia speaks to over the same protocol are a Config away,
// and the conformance suite already runs against several of them — but a
// provider nobody has credentials or an inventory entry for would be a claim
// this build cannot support, so only the one is registered.
func buildAdapters() ([]provider.Adapter, error) {
	baseURL := os.Getenv("RELAY_PROVIDER_OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	openai, err := openaicompat.New(openaicompat.Config{
		Provider: "openai",
		BaseURL:  baseURL,
		// Absent is a supported state: the adapter reports itself unconfigured
		// on the health surface rather than failing at the first request.
		APIKey: os.Getenv("RELAY_PROVIDER_OPENAI_API_KEY"),
	})
	if err != nil {
		return nil, err
	}
	return []provider.Adapter{openai}, nil
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

func intFromEnv(name string, fallback int64) int64 {
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
