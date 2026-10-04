// Package scopedpermit binds the private operation to source-reviewed authority.
// It never manufactures a principal, grants scopes or exempts catalogue policy.
package scopedpermit

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

// SourceReviewedAudience is one reviewed synthetic Alia operation, expiring
// 2026-10-05T02:30Z. It does not approve public availability or grant legal rights.
// There is no environment, public constructor or runtime configuration switch.
func SourceReviewedAudience() *contract.ScopedExecutionAudience {
	return &contract.ScopedExecutionAudience{
		PermitID:                  "jev-internal-synthetic-20261004-01",
		IdempotencyKey:            "jev-internal-synthetic-20261004-01",
		FixtureSHA256:             "023ad0e07ce8ad36de7b6c8b3a3e6817833e606b98a25a898e471fb1537d0125",
		ExpiresAt:                 "2026-10-05T02:30:00.000Z",
		DeploymentID:              "dep_openrouter_typesafe_jev_1_13_scoped_2026_10_04",
		Provider:                  "openrouter",
		KeyID:                     "b8090dce-82f2-4077-9fc1-fd831a53ca27",
		ModelReference:            "typesafe/jev-1.13@2026-09-17",
		UpstreamModelID:           "typesafe/jev-1.13-20260917",
		PriceVersionID:            "jev_scoped_price_20261004_01",
		ProviderRateCardVersionID: "rc_openrouter_jev_2026_10_04",
		ProviderSourceVersion:     "openrouter-api/2026-10-04/typesafe/jev-1.13-20260917/556fab0c5da201c07d4eeafd32b48250fb3fa297b69ed0f9e62a7225ca8511ba",
		MaxCostUSD:                "0.01",
		Principal:                 contract.ScopedExecutionPrincipal{AccountID: "01a0369b-1222-712f-8df6-f8ffeb78ccc2", ApplicationID: "6a2f851751b784a86fd0e934", CredentialID: "wl_d50a0056191d0448024900b4", Environment: contract.EnvironmentProduction},
		Policy:                    contract.RoutingPolicyReference{RoutingPolicyID: "platform-internal-default", PolicyVersion: 1},
	}
}

func Matches(actual, expected *contract.ScopedExecutionAudience, at time.Time) bool {
	return actual != nil && expected != nil && actual.Equal(expected) && actual.Validate() == nil && actual.NotExpired(at) && actual.Provider == "openrouter" && actual.UpstreamModelID == "typesafe/jev-1.13-20260917" && actual.ModelReference == "typesafe/jev-1.13@2026-09-17"
}
