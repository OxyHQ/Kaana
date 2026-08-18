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
	"sort"
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
	"github.com/OxyHQ/Relay/internal/providerconfig"
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

	providerConfigs, err := parseProviders(os.Getenv)
	if err != nil {
		return err
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

	// A credential delivered for a provider this process does not serve is the
	// one failure with no downstream signal at all: the task starts, the health
	// probe passes, the rollout completes and the provider is simply absent.
	if unused := unusedProviderCredentials(os.Environ(), providerConfigs); len(unused) > 0 {
		logger.Warn("a provider credential was delivered for a provider this process does not serve",
			"variables", unused,
			"declared", providerSlugsFrom(providerConfigs),
			"meaning", "the credential is never read; either add the provider to RELAY_PROVIDERS or remove the secret from the deployment")
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
//	RELAY_PROVIDERS                                   openai,openrouter,cerebras,anthropic
//	RELAY_PROVIDER_<SLUG>_PROTOCOL                    openai_compatible | anthropic_messages
//	RELAY_PROVIDER_<SLUG>_BASE_URL                    the provider's API root
//	RELAY_PROVIDER_<SLUG>_API_KEY                     one credential, or several separated by commas
//	RELAY_PROVIDER_<SLUG>_HEADERS                      Name=Value pairs the provider expects, separated by commas
//	RELAY_PROVIDER_<SLUG>_KEYS_ON_SEPARATE_ACCOUNTS   true when the keys are different provider accounts
//	RELAY_PROVIDER_<SLUG>_KEY_RETIREMENT              how long a spent or refused key stays out
//
// A whole POOL lives in the one _API_KEY variable rather than in numbered
// siblings, and that is a deployment property rather than a stylistic one: the
// variable name, the SSM parameter leaf and the GitHub secret name have to be
// one string for the deploy sync to work, and one name per provider keeps a
// growing pool out of the task definition's static secret list entirely.
// Adding a key becomes a parameter VALUE change; adding a provider stays one
// new name.
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
	// APIKeys is the provider's key pool, in the order it is to be spent. It is
	// the one field here that holds a secret, it comes from the environment,
	// and it is never logged, echoed or projected.
	APIKeys []string
	Keys    provider.KeyPolicy
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
	declared := providerconfig.SplitList(getenv("RELAY_PROVIDERS"))
	if len(declared) == 0 {
		return nil, errors.New("RELAY_PROVIDERS is required: it lists the provider slugs this process serves, and an empty one would leave every inventory route unservable")
	}

	configs := make([]providerConfig, 0, len(declared))
	seenSlug := make(map[contract.ProviderSlug]struct{}, len(declared))
	seenPrefix := make(map[string]contract.ProviderSlug, len(declared))

	var err error
	for _, name := range declared {
		slug := contract.ProviderSlug(name)
		if !slug.Valid() {
			return nil, fmt.Errorf("RELAY_PROVIDERS names %q, which is not a provider slug", name)
		}
		if _, duplicate := seenSlug[slug]; duplicate {
			return nil, fmt.Errorf("RELAY_PROVIDERS names %q twice", slug)
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

		switch config.Protocol {
		case providerconfig.ProtocolOpenAICompatible:
		case providerconfig.ProtocolAnthropicMessages:
			if slug != anthropic.Slug {
				// The Messages API adapter reports its slug as a constant,
				// because that wire format belongs to one provider. Serving it
				// under another name would attribute every event and every
				// usage record to `anthropic` while the inventory routed to
				// something else.
				return nil, fmt.Errorf("provider %q declares the %s protocol, which this build serves only as %q", slug, providerconfig.ProtocolAnthropicMessages, anthropic.Slug)
			}
		case "":
			return nil, fmt.Errorf("%s_PROTOCOL is required: this build has no protocol for the provider slug %q", prefix, slug)
		default:
			return nil, fmt.Errorf("%s_PROTOCOL is %q; this build speaks %s and %s", prefix, config.Protocol, providerconfig.ProtocolOpenAICompatible, providerconfig.ProtocolAnthropicMessages)
		}

		if config.BaseURL == "" {
			return nil, fmt.Errorf("%s_BASE_URL is required: this build knows no address for the provider slug %q", prefix, slug)
		}

		config.Headers, err = parseHeaders(getenv(prefix + "_HEADERS"))
		if err != nil {
			return nil, fmt.Errorf("%s_HEADERS: %w", prefix, err)
		}
		if len(config.Headers) > 0 && config.Protocol != providerconfig.ProtocolOpenAICompatible {
			// Only the chat-completions adapter carries extra headers. Accepting
			// them for another protocol would drop them silently, which is the
			// same failure as never having set them and harder to see.
			return nil, fmt.Errorf("%s_HEADERS is set, and the %s protocol sends no extra headers", prefix, config.Protocol)
		}

		onSeparateAccounts, err := parseDeclaredBool(getenv(prefix + "_KEYS_ON_SEPARATE_ACCOUNTS"))
		if err != nil {
			return nil, fmt.Errorf("%s_KEYS_ON_SEPARATE_ACCOUNTS: %w", prefix, err)
		}

		config.APIKeys = providerconfig.SplitList(getenv(prefix + "_API_KEY"))
		config.Keys = provider.KeyPolicy{
			Retirement:         durationOrZero(getenv(prefix + "_KEY_RETIREMENT")),
			OnSeparateAccounts: onSeparateAccounts,
		}
		configs = append(configs, config)
	}
	return configs, nil
}

// credentialHeaders are the header names an operator may not set here.
//
// Two different reasons, both silent if unguarded. An authentication header is
// applied by the adapter at send time and would be overwritten, so a credential
// placed here would look configured, do nothing, and sit in a plain environment
// variable outside the pool that exists to manage it. `anthropic-version` is
// pinned in code beside the parser that reads that provider's responses, and an
// operator changing it would change the JSON that package decodes.
var credentialHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"api-key":             {},
	"anthropic-version":   {},
}

// parseHeaders reads `Name=Value,Name=Value`.
//
// The separator is `=` rather than `:` because these values are routinely URLs,
// and a header value carrying a comma is not representable — said here rather
// than discovered by an operator whose attribution header arrived truncated.
func parseHeaders(value string) (map[string]string, error) {
	pairs := providerconfig.SplitList(value)
	if len(pairs) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name, headerValue, found := strings.Cut(pair, "=")
		name, headerValue = strings.TrimSpace(name), strings.TrimSpace(headerValue)
		switch {
		case !found || name == "":
			return nil, fmt.Errorf("%q is not a `Name=Value` pair", pair)
		case headerValue == "":
			return nil, fmt.Errorf("header %q has no value", name)
		}
		if _, refused := credentialHeaders[strings.ToLower(name)]; refused {
			return nil, fmt.Errorf("header %q is applied by the adapter and must not be set here", name)
		}
		if _, duplicate := headers[name]; duplicate {
			return nil, fmt.Errorf("header %q is declared twice", name)
		}
		headers[name] = headerValue
	}
	return headers, nil
}

// unusedProviderCredentials names credentials the process was handed for a
// provider it does not serve.
//
// It is the one failure in this area that nothing downstream can see. A
// credential missing from the deployment stops the task; a provider missing
// from the inventory refuses a request; but a key delivered for a provider that
// was never declared produces a process that starts, answers its health probe,
// reports its rollout complete, and routes to nothing — every signal green. The
// only place both halves are visible is here.
//
// It warns rather than refuses. Retiring a provider means removing it from this
// list before deleting its parameter, and a refusal would turn the safe order
// into the one that stops every task.
func unusedProviderCredentials(environment []string, configs []providerConfig) []string {
	declared := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		declared[providerconfig.EnvironmentPrefix(config.Slug)] = struct{}{}
	}

	unused := make([]string, 0)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || !strings.HasPrefix(name, "RELAY_PROVIDER_") || !strings.HasSuffix(name, "_API_KEY") {
			continue
		}
		if _, serves := declared[strings.TrimSuffix(name, "_API_KEY")]; !serves {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	return unused
}

// parseDeclaredBool reads a value an operator sets to state a fact about the
// deployment, and refuses anything it cannot read.
//
// It refuses rather than falling back for the same reason
// failoverAcknowledgement does: the variables in this family exist so a human
// states something the process cannot work out for itself, and a spelling that
// quietly means "not set" gives them the default while they believe they
// changed it. `TRUE` disabling key rotation, silently, is not a defensible
// answer to somebody who wrote `TRUE`.
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
				Provider: config.Slug,
				BaseURL:  config.BaseURL,
				APIKeys:  config.APIKeys,
				Keys:     config.Keys,
				Headers:  config.Headers,
			})
			if err != nil {
				return nil, err
			}
			adapters = append(adapters, adapter)
		case providerconfig.ProtocolAnthropicMessages:
			adapter, err := anthropic.New(anthropic.Config{
				BaseURL: config.BaseURL,
				APIKeys: config.APIKeys,
				Keys:    config.Keys,
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
