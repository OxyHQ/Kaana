// Command kaana runs the inference data plane.
//
// Provider credentials come from Kaana's PostgreSQL database and are decrypted
// by KMS only inside this process. The Oxy edge's key is a PUBLIC key, so Kaana
// cannot construct an envelope it would itself accept.
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

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/httpapi"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/provider/anthropic"
	"github.com/OxyHQ/Kaana/internal/provider/openaicompat"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("kaana could not start", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	inventoryPath := os.Getenv("KAANA_INVENTORY_PATH")
	if inventoryPath == "" {
		return errors.New("KAANA_INVENTORY_PATH is required: without a deployment inventory nothing can be routed")
	}
	inventoryStore, err := inventory.NewStore(inventory.Config{
		Path:           inventoryPath,
		MaxSnapshotAge: durationFromEnv("KAANA_INVENTORY_MAX_AGE", inventory.DefaultMaxSnapshotAge),
		Logger:         logger,
	})
	if err != nil {
		return err
	}

	// Upstream rate cards are optional and hold no customer-facing amount. An
	// absent file means provider cost is not measured, which every measurement
	// then says rather than reporting zero.
	var costs *providercost.Cards
	if ratesPath := os.Getenv("KAANA_PROVIDER_RATES_PATH"); ratesPath != "" {
		costs, err = providercost.Load(ratesPath)
		if err != nil {
			return err
		}
	}

	keys, err := edgeauth.ParsePublicKeys(os.Getenv("KAANA_EDGE_PUBLIC_KEYS"))
	if err != nil {
		return fmt.Errorf("KAANA_EDGE_PUBLIC_KEYS: %w", err)
	}
	verifier, err := edgeauth.NewVerifier(keys, durationFromEnv("KAANA_EDGE_MAX_SKEW", edgeauth.DefaultMaxSkew))
	if err != nil {
		return err
	}

	providerConfigs, err := parseProviders(os.Getenv)
	if err != nil {
		return err
	}
	credentialContext, cancelCredentialLoad := context.WithTimeout(context.Background(), 45*time.Second)
	credentialStore, credentialDatabase, err := credentialstore.Open(
		credentialContext,
		strings.TrimSpace(os.Getenv("DATABASE_URL")),
		strings.TrimSpace(os.Getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN")),
	)
	if err != nil {
		cancelCredentialLoad()
		return err
	}
	declarations, unenforcedBudgets, err := credentialStore.Load(credentialContext, providerSlugsFrom(providerConfigs))
	cancelCredentialLoad()
	if err != nil {
		credentialDatabase.Close()
		return err
	}
	defer credentialDatabase.Close()
	for index := range providerConfigs {
		providerConfigs[index].Declarations = declarations[providerConfigs[index].Slug]
	}
	logger.Info("provider credentials loaded from Kaana's database", "providers", len(providerConfigs))
	if len(unenforcedBudgets) > 0 {
		logger.Warn("a declared per-key budget is not enforced by this build",
			"keys", unenforcedBudgets,
			"meaning", "these keys will keep serving past the amount declared for them; nothing here holds them to it")
	}
	adapters, err := buildAdapters(providerConfigs)
	if err != nil {
		return err
	}
	registry, err := provider.NewRegistry(adapters...)
	if err != nil {
		return err
	}

	// A snapshot naming a provider this process does not serve is a
	// degradation, not a reason to stop.
	//
	// The inventory is published by the control plane and the adapter set is
	// fixed at deploy time, so the two move on different clocks: a provider can
	// appear in a snapshot before the deploy that gives this build its
	// credential. Refusing to start there would take routing for every
	// SUPPORTED provider down over one unsupported one, and it would do it on
	// the next task replacement rather than when the snapshot changed — the
	// reload path already treats the same condition as a warning, so the two
	// answers were the same question answered twice.
	//
	// What the customer gets instead is the refusal the executor already
	// produces: a reference served only by an unroutable provider resolves to
	// no admissible candidate and is refused, retryably, while every other
	// reference is served. The operator gets this line.
	warnAboutUnroutableProviders(logger, inventoryStore.Current(), registry)

	rotationRegistry := rotation.NewRegistry(rotation.Policy{
		FailuresToOpen:   intFromEnv("KAANA_BREAKER_FAILURES_TO_OPEN", 0),
		Cooldown:         durationFromEnv("KAANA_BREAKER_COOLDOWN", 0),
		MaxCooldown:      durationFromEnv("KAANA_BREAKER_MAX_COOLDOWN", 0),
		SuccessesToClose: intFromEnv("KAANA_BREAKER_SUCCESSES_TO_CLOSE", 0),
	}, nil)

	executor, err := kaana.NewExecutor(kaana.Config{
		Inventory: inventoryStore,
		Providers: registry,
		Rotation:  rotationRegistry,
		Costs:     costs,
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
		MaxEnvelopeBytes: int64FromEnv("KAANA_MAX_ENVELOPE_BYTES", httpapi.DefaultMaxEnvelopeBytes),
	})
	if err != nil {
		return err
	}

	address := os.Getenv("KAANA_ADDR")
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

	logger.Info("kaana is listening",
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
		durationFromEnv("KAANA_INVENTORY_RELOAD_INTERVAL", 30*time.Second))
	go reloadCredentialPools(ctx, credentialStore, providerConfigs, registry, logger,
		durationFromEnv("KAANA_CREDENTIAL_RELOAD_INTERVAL", time.Minute))

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
		logger.Info("kaana is draining")
		// In-flight generations finish; nothing new is accepted. A shorter
		// drain would cut streams a customer is already being charged for.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func reloadCredentialPools(
	ctx context.Context,
	store *credentialstore.Store,
	configs []providerConfig,
	registry *provider.Registry,
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
			declarations, unenforcedBudgets, err := store.Load(loadContext, providerSlugsFrom(configs))
			cancel()
			if err != nil {
				logger.Error("provider credentials could not be reloaded; keeping the last complete pools", "error", err)
				continue
			}
			replacementConfigs := append([]providerConfig(nil), configs...)
			for index := range replacementConfigs {
				replacementConfigs[index].Declarations = declarations[replacementConfigs[index].Slug]
			}
			adapters, err := buildAdapters(replacementConfigs)
			if err != nil {
				logger.Error("reloaded provider credentials could not build a complete adapter set; keeping the previous pools", "error", err)
				continue
			}
			if err := registry.Replace(adapters...); err != nil {
				logger.Error("reloaded provider credentials could not replace the adapter registry; keeping the previous pools", "error", err)
				continue
			}
			logger.Info("provider credentials reloaded from Kaana's database", "providers", len(replacementConfigs))
			if len(unenforcedBudgets) > 0 {
				logger.Warn("a declared per-key budget is not enforced by this build", "keys", unenforcedBudgets)
			}
		}
	}
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
			// The same call the startup path makes, deliberately: one condition
			// with two spellings is one condition an alarm can only half see,
			// and this is the signal an operator has left for a snapshot that
			// routes somewhere this build cannot reach.
			warnAboutUnroutableProviders(logger, current, adapters)
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

// Provider configuration.
//
// Which providers this process serves, where each one is, and what credentials
// it holds are three separate questions, and none of them is the inventory's.
// The inventory is a control-plane snapshot of which deployments serve which
// model; a credential in it would be a copy of an Oxy entity, and a base URL in
// it would make one process's reachability a global fact. So the inventory
// names a provider SLUG and this file resolves that slug to an adapter, an
// address and a pool of keys.
//
//	KAANA_PROVIDERS                                   openai,openrouter,cerebras,anthropic
//	KAANA_PROVIDER_<SLUG>_PROTOCOL                    openai_compatible | anthropic_messages
//	KAANA_PROVIDER_<SLUG>_BASE_URL                    the provider's API root
//	KAANA_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS   true when the keys are different provider accounts
//	KAANA_PROVIDER_<SLUG>_KEY_RETIREMENT              how long a spent or refused key stays out
//
// Provider secrets are deliberately absent from this environment contract.
// The pool is loaded from PostgreSQL and decrypted through KMS after this
// non-secret adapter configuration has been validated.
//
// The one closed list in any of this is the PROTOCOL. It names which adapter
// implementation to construct, and a build can only construct one it contains,
// so an unknown value is refused rather than defaulted. Provider SLUGS are not
// a closed list: `providerconfig.Known` is a defaults table, and any slug that
// declares a protocol and a base URL is servable without a Go change.
//
// The protocol names, the defaults table and the environment prefix live in
// `internal/providerconfig` because the publisher command resolves the same
// address from the same variable, and two copies of an address drift silently.

// providerConfig is one provider this process serves.
type providerConfig struct {
	Slug     contract.ProviderSlug
	Protocol string
	BaseURL  string
	// Declarations is the provider's key pool, in the order it was declared,
	// each key carrying what the operator says it costs. It is populated only
	// from the encrypted credential store and is never logged or projected.
	Declarations []provider.KeyDeclaration
	Keys         provider.KeyPolicy
	// Headers are the non-secret headers this provider expects on every
	// request. OpenRouter's attribution headers are the reason the field
	// exists, and a provider wired without them is compliant until it is not,
	// which is worse than being wired wrong.
	Headers map[string]string
}

// parseProviders reads the provider configuration from the environment.
//
// It takes its own getenv so the whole of it is testable without a process
// environment, which matters because most of what it does is refuse.
func parseProviders(getenv func(string) string) ([]providerConfig, error) {
	declared := providerconfig.SplitList(getenv("KAANA_PROVIDERS"))
	if len(declared) == 0 {
		return nil, errors.New("KAANA_PROVIDERS is required: it lists the provider slugs this process serves, and an empty one would leave every inventory route unservable")
	}

	configs := make([]providerConfig, 0, len(declared))
	seenSlug := make(map[contract.ProviderSlug]struct{}, len(declared))
	seenPrefix := make(map[string]contract.ProviderSlug, len(declared))

	for _, name := range declared {
		slug := contract.ProviderSlug(name)
		if !slug.Valid() {
			return nil, fmt.Errorf("KAANA_PROVIDERS names %q, which is not a provider slug", name)
		}
		if _, duplicate := seenSlug[slug]; duplicate {
			return nil, fmt.Errorf("KAANA_PROVIDERS names %q twice", slug)
		}
		seenSlug[slug] = struct{}{}

		prefix := providerconfig.EnvironmentPrefix(slug)
		if other, collides := seenPrefix[prefix]; collides {
			// `open-router` and `open.router` are two slugs and one variable
			// name. Left alone, the second would silently be configured with
			// the first one's address and credentials.
			return nil, fmt.Errorf("providers %q and %q both read their configuration from %s_*", other, slug, prefix)
		}
		seenPrefix[prefix] = slug

		known := providerconfig.Known[slug]
		config := providerConfig{Slug: slug, Protocol: known.Protocol, BaseURL: known.BaseURL}
		if declaredProtocol := strings.TrimSpace(getenv(prefix + "_PROTOCOL")); declaredProtocol != "" {
			config.Protocol = declaredProtocol
		}
		if declaredBaseURL := strings.TrimSpace(getenv(prefix + "_BASE_URL")); declaredBaseURL != "" {
			config.BaseURL = declaredBaseURL
		}

		if strings.TrimSpace(getenv(prefix+"_HEADERS")) != "" {
			return nil, fmt.Errorf("%s_HEADERS is not supported: public provider metadata is reviewed in the Kaana binary and provider authentication comes only from the encrypted credential store", prefix)
		}
		config.Headers = reviewedHeaders(slug)
		// One authority for what a provider configuration must satisfy. Two
		// copies of "which protocols this build speaks" drift, and the drift is
		// invisible: the path nobody exercised is the one that accepts what the
		// other refuses.
		if err := validateProvider(&config, prefix+"_*"); err != nil {
			return nil, err
		}

		onSeparateAccounts, err := parseDeclaredBool(getenv(prefix + "_KEYS_ON_SEPARATE_ACCOUNTS"))
		if err != nil {
			return nil, fmt.Errorf("%s_KEYS_ON_SEPARATE_ACCOUNTS: %w", prefix, err)
		}

		config.Keys = provider.KeyPolicy{
			Retirement:         durationOrZero(getenv(prefix + "_KEY_RETIREMENT")),
			OnSeparateAccounts: onSeparateAccounts,
		}
		configs = append(configs, config)
	}
	return configs, nil
}

// reviewedProviderHeaders are public product identity, compiled into the
// binary rather than accepted from environment. Even an allow-listed header
// name is not enough: `X-Title=sk-...` would still put a provider key in an ECS
// task definition. New metadata therefore requires a code review, while all
// provider authentication continues to come from PostgreSQL/KMS.
var reviewedProviderHeaders = map[contract.ProviderSlug]map[string]string{
	"openrouter": {
		"HTTP-Referer": "https://oxy.so",
		"X-Title":      "Oxy",
	},
}

func reviewedHeaders(slug contract.ProviderSlug) map[string]string {
	reviewed := reviewedProviderHeaders[slug]
	if len(reviewed) == 0 {
		return nil
	}
	headers := make(map[string]string, len(reviewed))
	for name, value := range reviewed {
		headers[name] = value
	}
	return headers
}

// parseDeclaredBool reads a value an operator sets to state a fact about the
// deployment, and refuses anything it cannot read.
//
// It refuses rather than falling back because the variables in this family
// state facts the process cannot work out for itself, and a spelling that
// quietly means "not set" gives them the default while an operator believes
// they changed it. `TRUE` disabling key rotation, silently, is not defensible.
func parseDeclaredBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return false, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%q is neither `true` nor `false`", strings.TrimSpace(value))
	}
}

func durationOrZero(value string) time.Duration {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// buildAdapters constructs every provider this process serves.
//
// Registering an adapter is not a claim that a credential exists for it — a
// provider with an empty pool reports itself `unconfigured` on the health
// surface rather than failing at the first request, which is what lets an
// operator see the gap before a customer does. What would be a claim this build
// cannot support is an INVENTORY entry routing to a provider that was never
// declared here, and the server refuses to start in that state.
func buildAdapters(configs []providerConfig) ([]provider.Adapter, error) {
	adapters := make([]provider.Adapter, 0, len(configs))
	for _, config := range configs {
		switch config.Protocol {
		case providerconfig.ProtocolOpenAICompatible:
			adapter, err := openaicompat.New(openaicompat.Config{
				Provider:     config.Slug,
				BaseURL:      config.BaseURL,
				Declarations: config.Declarations,
				Keys:         config.Keys,
				Headers:      config.Headers,
			})
			if err != nil {
				return nil, err
			}
			adapters = append(adapters, adapter)
		case providerconfig.ProtocolAnthropicMessages:
			adapter, err := anthropic.New(anthropic.Config{
				BaseURL:      config.BaseURL,
				Declarations: config.Declarations,
				Keys:         config.Keys,
			})
			if err != nil {
				return nil, err
			}
			adapters = append(adapters, adapter)
		default:
			return nil, fmt.Errorf("provider %q declares the protocol %q, which parseProviders should have refused", config.Slug, config.Protocol)
		}
	}
	return adapters, nil
}

// warnAboutUnroutableProviders names every provider the installed snapshot
// routes to that this build cannot reach.
//
// Startup and reload both call it, and that is the whole reason it is a
// function: the condition is identical on both paths, it is not fatal on
// either, and the only instrument anyone has for it is a log filter — which a
// second spelling of the same message would silently half-miss.
func warnAboutUnroutableProviders(logger *slog.Logger, current *inventory.Inventory, registry *provider.Registry) {
	unroutable := make([]contract.ProviderSlug, 0)
	for _, slug := range current.Providers() {
		if _, found := registry.Lookup(slug); !found {
			unroutable = append(unroutable, slug)
		}
	}
	if len(unroutable) == 0 {
		return
	}
	logger.Warn("the installed snapshot routes to providers this build has no adapter for",
		"providers", unroutable,
		"snapshotId", current.SnapshotID(),
		"meaning", "references served only by those providers are refused; every other reference is served normally")
}

func providerSlugsFrom(configs []providerConfig) []contract.ProviderSlug {
	slugs := make([]contract.ProviderSlug, 0, len(configs))
	for _, config := range configs {
		slugs = append(slugs, config.Slug)
	}
	return slugs
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

// validateProvider holds the adapter configuration rules. `source` names where
// the configuration came from, so an error sends the reader to the right place.
func validateProvider(config *providerConfig, source string) error {
	switch config.Protocol {
	case providerconfig.ProtocolOpenAICompatible:
	case providerconfig.ProtocolAnthropicMessages:
		if config.Slug != anthropic.Slug {
			// The Messages API adapter reports its slug as a constant, because
			// that wire format belongs to one provider. Serving it under another
			// name would attribute every event and every usage record to
			// `anthropic` while the inventory routed to something else.
			return fmt.Errorf("%s: provider %q declares the %s protocol, which this build serves only as %q", source, config.Slug, providerconfig.ProtocolAnthropicMessages, anthropic.Slug)
		}
	case "":
		return fmt.Errorf("%s: provider %q declares no protocol and this build has no default for that slug", source, config.Slug)
	default:
		return fmt.Errorf("%s: provider %q declares protocol %q; this build speaks %s and %s", source, config.Slug, config.Protocol, providerconfig.ProtocolOpenAICompatible, providerconfig.ProtocolAnthropicMessages)
	}

	if config.BaseURL == "" {
		return fmt.Errorf("%s: provider %q declares no base URL and this build knows no address for that slug", source, config.Slug)
	}
	if err := providerconfig.ValidateBaseURL(config.BaseURL); err != nil {
		return fmt.Errorf("%s: provider %q: %w", source, config.Slug, err)
	}

	for name := range config.Headers {
		value, allowed := reviewedProviderHeaders[config.Slug][name]
		if !allowed || value != config.Headers[name] {
			return fmt.Errorf("%s: provider %q sets header %q, which is not reviewed public configuration", source, config.Slug, name)
		}
	}
	if len(config.Headers) > 0 && config.Protocol != providerconfig.ProtocolOpenAICompatible {
		// Only the chat-completions adapter carries extra headers. Accepting
		// them for another protocol would drop them silently, which is the same
		// failure as never having set them and harder to see.
		return fmt.Errorf("%s: provider %q sets headers, and the %s protocol sends no extra headers", source, config.Slug, config.Protocol)
	}
	return nil
}
