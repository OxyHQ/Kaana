// Package scopedpermit binds the private operation to source-reviewed authority.
// It never manufactures a principal, grants scopes or exempts catalogue policy.
package scopedpermit

import (
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

// SourceReviewedAudience is one reviewed own-Mention native English operation.
// The retired Alia operation is never renewed; expiry is frozen independently.
// It does not approve public availability or grant legal rights.
// There is no environment, public constructor or runtime configuration switch.
func SourceReviewedAudience() *contract.ScopedExecutionAudience {
	return &contract.ScopedExecutionAudience{
		PermitID:                  "jev-mention-native-en-onepost-20261005-01",
		IdempotencyKey:            "mention_jev_native_en_8d04b9d17510fe89d7ae084039ee4231",
		FixtureSHA256:             "a962e5ed49a962db7934684c62834dd25b04aba94162ad9d07140c9a2abeb3b2",
		ExpiresAt:                 "2026-10-05T03:14:23Z",
		DeploymentID:              "dep_openrouter_typesafe_jev_1_13_mention_native_2026_10_05",
		Provider:                  "openrouter",
		KeyID:                     "b8090dce-82f2-4077-9fc1-fd831a53ca27",
		ModelReference:            "typesafe/jev-1.13@2026-09-17",
		UpstreamModelID:           "typesafe/jev-1.13-20260917",
		PriceVersionID:            "jev_scoped_price_20261004_01",
		ProviderRateCardVersionID: "rc_openrouter_jev_mention_native_20261005_01",
		ProviderSourceVersion:     "openrouter-api/2026-10-04/typesafe/jev-1.13-20260917/556fab0c5da201c07d4eeafd32b48250fb3fa297b69ed0f9e62a7225ca8511ba",
		MaxCostUSD:                "0.01",
		Principal:                 contract.ScopedExecutionPrincipal{AccountID: "69b2d3df5d12f58c9800d651", ApplicationID: "6a2f851751b784a86fd0e916", CredentialID: "wl_d61be5cd068abb658ed4d193", Environment: contract.EnvironmentProduction},
		Policy:                    contract.RoutingPolicyReference{RoutingPolicyID: "platform-internal-default", PolicyVersion: 1},
	}
}

func Matches(actual, expected *contract.ScopedExecutionAudience, at time.Time) bool {
	return actual != nil && expected != nil && actual.Equal(expected) && actual.Validate() == nil && actual.NotExpired(at) && actual.Provider == "openrouter" && actual.UpstreamModelID == "typesafe/jev-1.13-20260917" && actual.ModelReference == "typesafe/jev-1.13@2026-09-17"
}
