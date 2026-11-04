package main

import (
	"context"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
)

// noMint is a workload identity that is never asked for a token here.
type noMint struct{}

func (noMint) Mint(context.Context, *url.URL, contract.OxyAudience) (string, time.Duration, error) {
	return "", 0, context.Canceled
}

// The control routes are derived from KAANA_OXY_API_BASE_URL. A deployment that
// still sets the retired variables boots only if each one IS the derived route
// (oxy-infra's app-kaana.tf sets exactly these); any other value fails the boot.
func TestRetiredControlURLsMustBeTheDerivedRoutes(t *testing.T) {
	reporter, err := oxyvalidation.New(oxyvalidation.Config{
		BaseURL: "https://api.oxy.so", WorkloadMinter: noMint{}, Environment: contract.EnvironmentProduction,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("oxyvalidation.New: %v", err)
	}
	defer func() { _ = reporter.Close(context.Background()) }()

	const authorizations = "https://api.oxy.so/internal/kaana/v1/authorizations"
	const settlements = "https://api.oxy.so/internal/kaana/v1/settlements"
	for _, tc := range []struct {
		name          string
		authorization string
		settlement    string
		accepted      bool
	}{
		{"neither set", "", "", true},
		{"the deployed values", authorizations, settlements, true},
		{"only one set", authorizations, "", true},
		{"surrounding whitespace, trimmed as before", " " + authorizations + "\n", settlements, true},

		{"another origin", "https://attacker.example/internal/kaana/v1/authorizations", settlements, false},
		{"a look-alike host", "https://api.oxy.so.attacker.example/internal/kaana/v1/authorizations", settlements, false},
		{"plain http", "http://api.oxy.so/internal/kaana/v1/authorizations", settlements, false},
		{"another port", "https://api.oxy.so:8443/internal/kaana/v1/authorizations", settlements, false},
		{"userinfo", "https://kaana@api.oxy.so/internal/kaana/v1/authorizations", settlements, false},
		{"a query", authorizations + "?next=https://attacker.example", settlements, false},
		{"a fragment", authorizations + "#x", settlements, false},
		{"another route on Oxy", "https://api.oxy.so/internal/kaana/v1/settlements", settlements, false},
		{"the settlement route elsewhere", authorizations, "https://attacker.example/internal/kaana/v1/settlements", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(name string) string {
				switch name {
				case "KAANA_OXY_AUTHORIZATION_URL":
					return tc.authorization
				case "KAANA_OXY_SETTLEMENT_URL":
					return tc.settlement
				}
				return ""
			}
			err := requireDerivedControlURLs(getenv, reporter)
			if accepted := err == nil; accepted != tc.accepted {
				t.Fatalf("accepted = %v (%v), want %v", accepted, err, tc.accepted)
			}
		})
	}
}
