package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/OxyHQ/Kaana/internal/emf"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/OxyHQ/Kaana/internal/publicapi"
	"github.com/OxyHQ/Kaana/internal/realtime"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

// publicAPIDependencies is what the public API is built from.
type publicAPIDependencies struct {
	catalogue *publicapi.PublicCatalogue
	costs     *providercost.Cards
	executor  *kaana.Executor
	inventory *inventory.Store
	registry  *provider.Registry
	rotation  *rotation.Registry
	metrics   emf.Recorder
	sessions  *realtime.Manager
	store     *credentialstore.Postgres
	keys      map[string]ed25519.PublicKey
	// oxy is Kaana's access to Oxy's /internal/kaana/v1/*: the token for it
	// (audience oxy-api:kaana-control; an oxy-api token is refused there with
	// 401) and each route on the Oxy origin KAANA_OXY_API_BASE_URL names.
	oxy    publicapi.OxyControl
	logger *slog.Logger
	getenv func(string) string
}

// publicAPI is the public surface, when it is enabled.
type publicAPI struct {
	closeNative    func()
	server         *publicapi.Server
	settler        *publicapi.Settler
	reconciliation http.Handler
}

// route puts /v1 and the settlement reconciliation read in front of the
// internal surface. Disabled, it is the internal surface alone, exactly as
// before.
func (p publicAPI) route(internal http.Handler) http.Handler {
	if p.server == nil {
		return internal
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", p.server.Handler())
	mux.Handle("GET /internal/v1/settlements", p.reconciliation)
	mux.Handle("/", internal)
	return mux
}

// buildPublicAPI reads the public API's configuration. Every value it needs is
// required once it is enabled: a public API that could not ask Oxy for a grant
// or settle what it served must not start.
func buildPublicAPI(deps publicAPIDependencies) (publicAPI, error) {
	getenv := deps.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(orDefault(getenv("KAANA_PUBLIC_API_ENABLED"), "false")))
	if err != nil {
		return publicAPI{}, errors.New("KAANA_PUBLIC_API_ENABLED must be true or false")
	}
	if !enabled {
		return publicAPI{}, nil
	}
	required := func(name string) (string, error) {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required when KAANA_PUBLIC_API_ENABLED is true", name)
		}
		return value, nil
	}
	signingKey, err := required("KAANA_SETTLEMENT_SIGNING_KEY")
	if err != nil {
		return publicAPI{}, err
	}
	signer, err := edgeauth.NewSettlementSigner(signingKey)
	if err != nil {
		return publicAPI{}, err
	}
	grants, err := edgeauth.NewGrantVerifier(deps.keys, edgeauth.DefaultMaxSkew)
	if err != nil {
		return publicAPI{}, err
	}
	reads, err := edgeauth.NewSettlementReadVerifier(deps.keys, edgeauth.DefaultMaxSkew)
	if err != nil {
		return publicAPI{}, err
	}
	authorizer, err := publicapi.NewOxyAuthorizer(publicapi.OxyAuthorizerConfig{
		Oxy: deps.oxy, Grants: grants, Logger: deps.logger,
		Timeout: durationFromEnv("KAANA_OXY_AUTHORIZATION_TIMEOUT", 10*time.Second),
	})
	if err != nil {
		return publicAPI{}, err
	}
	settler, err := publicapi.NewSettler(publicapi.SettlerConfig{
		Store: deps.store, Oxy: deps.oxy, Signer: signer, Logger: deps.logger, Metrics: deps.metrics,
	})
	if err != nil {
		return publicAPI{}, err
	}
	if err := requireDerivedControlURLs(getenv, deps.oxy); err != nil {
		return publicAPI{}, err
	}
	var origins []string
	for _, origin := range strings.Split(getenv("KAANA_CORS_ALLOWED_ORIGINS"), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			origins = append(origins, origin)
		}
	}
	rate := publicapi.DefaultRateLimit
	if raw := strings.TrimSpace(getenv("KAANA_PUBLIC_RATE_PER_SECOND")); raw != "" {
		perSecond, err := strconv.ParseFloat(raw, 64)
		if err != nil || perSecond <= 0 {
			return publicAPI{}, errors.New("KAANA_PUBLIC_RATE_PER_SECOND must be a positive number")
		}
		rate = publicapi.RateLimit{PerSecond: perSecond, Burst: max(1, int(perSecond*2))}
	}
	catalogue := deps.catalogue
	if catalogue == nil {
		return publicAPI{}, errors.New("the declared catalogue is required when KAANA_PUBLIC_API_ENABLED is true")
	}
	native, closeNative, err := buildNativeAPI(deps, getenv)
	if err != nil {
		return publicAPI{}, err
	}
	server, err := publicapi.New(publicapi.Config{
		Native:    native,
		Catalogue: catalogue, Costs: deps.costs,
		Executor: deps.executor, Authorizer: authorizer, Settler: settler, Inventory: deps.inventory,
		Funding: publicapi.RegistryFunding{Registry: deps.registry}, Latency: deps.rotation, Metrics: deps.metrics, Kinds: deps.registry, AllowedOrigins: origins,
		RateLimit: rate, Logger: deps.logger,
	})
	if err != nil {
		closeNative()
		return publicAPI{}, err
	}
	server.UseRealtime(deps.sessions.PublicHandler(server.RealtimeGate()))
	deps.logger.Info("the public API is enabled",
		"authorizationUrl", deps.oxy.ControlURL(contract.OxyKaanaAuthorizationsEndpoint),
		"settlementUrl", deps.oxy.ControlURL(contract.OxyKaanaSettlementsEndpoint),
		"settlementKeyId", signer.KeyID(), "corsOrigins", origins)
	return publicAPI{
		server: server, settler: settler, closeNative: closeNative,
		reconciliation: publicapi.ReconciliationHandler{Store: deps.store, Verifier: reads, Logger: deps.logger},
	}, nil
}

// retiredControlURLs are the variables that once named Oxy's control routes
// on their own. Each route is now its fixed path on KAANA_OXY_API_BASE_URL's
// validated origin, the one the control token is minted by, so neither is
// read. A deployment that still sets one is held to the derived value: a
// mismatch fails the boot rather than letting an operator believe the token
// goes where the variable says.
var retiredControlURLs = []struct {
	name     string
	endpoint contract.OxyKaanaControlEndpoint
}{
	{"KAANA_OXY_AUTHORIZATION_URL", contract.OxyKaanaAuthorizationsEndpoint},
	{"KAANA_OXY_SETTLEMENT_URL", contract.OxyKaanaSettlementsEndpoint},
}

func requireDerivedControlURLs(getenv func(string) string, oxy publicapi.OxyControl) error {
	for _, retired := range retiredControlURLs {
		value := strings.TrimSpace(getenv(retired.name))
		if value == "" {
			continue
		}
		if derived := oxy.ControlURL(retired.endpoint); value != derived {
			return fmt.Errorf("%s is %q, but the route is %q: it is derived from KAANA_OXY_API_BASE_URL; remove %s",
				retired.name, value, derived, retired.name)
		}
	}
	return nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
