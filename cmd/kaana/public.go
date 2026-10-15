package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/kaana"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/publicapi"
	"github.com/OxyHQ/Kaana/internal/realtime"
)

// publicAPIDependencies is what the public API is built from.
type publicAPIDependencies struct {
	executor  *kaana.Executor
	inventory *inventory.Store
	registry  *provider.Registry
	sessions  *realtime.Manager
	store     *credentialstore.Postgres
	keys      map[string]ed25519.PublicKey
	token     publicapi.TokenSource
	logger    *slog.Logger
	getenv    func(string) string
}

// publicAPI is the public surface, when it is enabled.
type publicAPI struct {
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
	authorizationURL, err := required("KAANA_OXY_AUTHORIZATION_URL")
	if err != nil {
		return publicAPI{}, err
	}
	settlementURL, err := required("KAANA_OXY_SETTLEMENT_URL")
	if err != nil {
		return publicAPI{}, err
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
		URL: authorizationURL, Token: deps.token, Grants: grants, Logger: deps.logger,
		Timeout: durationFromEnv("KAANA_OXY_AUTHORIZATION_TIMEOUT", 10*time.Second),
	})
	if err != nil {
		return publicAPI{}, err
	}
	settler, err := publicapi.NewSettler(publicapi.SettlerConfig{
		Store: deps.store, URL: settlementURL, Token: deps.token, Signer: signer, Logger: deps.logger,
	})
	if err != nil {
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
	catalogue, err := publicapi.LoadPublicCatalogue(getenv("KAANA_PUBLIC_CATALOGUE_PATH"))
	if err != nil {
		return publicAPI{}, err
	}
	server, err := publicapi.New(publicapi.Config{
		Catalogue: catalogue,
		Executor:  deps.executor, Authorizer: authorizer, Settler: settler, Inventory: deps.inventory,
		Funding: publicapi.RegistryFunding{Registry: deps.registry}, AllowedOrigins: origins,
		RateLimit: rate, Logger: deps.logger,
	})
	if err != nil {
		return publicAPI{}, err
	}
	server.UseRealtime(deps.sessions.PublicHandler(server.RealtimeGate()))
	deps.logger.Info("the public API is enabled",
		"authorizationUrl", authorizationURL, "settlementUrl", settlementURL,
		"settlementKeyId", signer.KeyID(), "corsOrigins", origins)
	return publicAPI{
		server: server, settler: settler,
		reconciliation: publicapi.ReconciliationHandler{Store: deps.store, Verifier: reads, Logger: deps.logger},
	}, nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
