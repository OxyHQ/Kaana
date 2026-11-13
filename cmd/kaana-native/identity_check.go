package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/edgeauth"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
	"github.com/OxyHQ/Kaana/internal/publicapi"
	"github.com/OxyHQ/Kaana/internal/workloadidentity"
)

type identityCheckClient struct {
	mint   func(context.Context) (string, error)
	verify publicapi.IdentityVerifier
	close  func(context.Context) error
}

type identityCheckOpener func(context.Context, func(string) string) (identityCheckClient, error)

// This canary checks Kaana's own workload, never a customer's credential. It
// opens no database and never constructs an inference or economic request.
func runIdentityCheck(args []string, output io.Writer, getenv func(string) string, open identityCheckOpener) error {
	flags := flag.NewFlagSet("identity-check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	applicationID := flags.String("application-id", "", "reviewed Kaana application identity")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*applicationID) == "" {
		return errors.New("identity-check requires the reviewed application ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client, err := open(ctx, getenv)
	if err != nil {
		return errors.New("identity-check configuration unavailable")
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		closeContext, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		_ = client.close(closeContext)
	}()
	credential, err := client.mint(ctx)
	if err != nil || credential == "" {
		return errors.New("identity-check workload authentication refused")
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return errors.New("identity-check request identity unavailable")
	}
	assertion, refusal := client.verify.Verify(ctx, contract.IdentityRequest{
		SchemaVersion: 1, Kind: "identity", RequestID: contract.RequestID("identity-check-" + hex.EncodeToString(nonce)),
		Audience: "kaana.ai", Credential: credential,
	})
	if refusal != nil || assertion == nil {
		return errors.New("identity-check signed assertion refused")
	}
	principal := assertion.Principal
	if string(principal.ApplicationID) != *applicationID || principal.Authority != contract.IdentityAuthorityFirstParty || principal.Lane != contract.IdentityLaneService || principal.Environment != contract.EnvironmentProduction || assertion.UserID != nil || len(principal.Scopes) != 0 {
		return errors.New("identity-check workload assertion does not match reviewed identity")
	}
	if err := client.close(ctx); err != nil {
		return errors.New("identity-check cleanup incomplete")
	}
	closed = true
	return json.NewEncoder(output).Encode(struct {
		Verified bool `json:"verified"`
	}{Verified: true})
}

func openIdentityCheck(ctx context.Context, getenv func(string) string) (identityCheckClient, error) {
	// The public signing keys are configuration; no private signing key, database
	// URL, provider credential or settlement configuration is read by this path.
	baseURL := strings.TrimSpace(getenv("KAANA_OXY_API_BASE_URL"))
	if baseURL != "https://api.oxy.so" || getenv("KAANA_OXY_SERVICE_ENVIRONMENT") != "production" {
		return identityCheckClient{}, errors.New("identity-check requires canonical production identity configuration")
	}
	keys, err := edgeauth.ParsePublicKeys(getenv("KAANA_EDGE_PUBLIC_KEYS"))
	if err != nil {
		return identityCheckClient{}, err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	workload, err := workloadidentity.Open(ctx, workloadidentity.Config{Logger: logger})
	if err != nil {
		return identityCheckClient{}, err
	}
	// oxyvalidation.New owns its bounded background queue; Close joins it.
	// Request contexts are passed to Verify and Mint below.
	//nolint:contextcheck // Constructor has no context argument; its explicit Close lifecycle is always joined.
	reporter, err := oxyvalidation.New(oxyvalidation.Config{BaseURL: baseURL, Environment: contract.EnvironmentProduction, WorkloadMinter: workload, Logger: logger, Timeout: 10 * time.Second})
	if err != nil {
		return identityCheckClient{}, err
	}
	verifier, err := publicapi.NewOxyIdentityVerifier(publicapi.OxyIdentityConfig{Oxy: reporter, Keys: keys, Logger: logger, Timeout: 10 * time.Second})
	if err != nil {
		_ = reporter.Close(ctx)
		return identityCheckClient{}, err
	}
	origin, err := url.Parse(baseURL)
	if err != nil {
		_ = reporter.Close(ctx)
		return identityCheckClient{}, err
	}
	return identityCheckClient{
		mint: func(ctx context.Context) (string, error) {
			token, _, err := workload.Mint(ctx, origin, contract.OxyAudience("kaana.ai"))
			return token, err
		}, verify: verifier, close: reporter.Close,
	}, nil
}
