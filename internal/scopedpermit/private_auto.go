package scopedpermit

import "github.com/OxyHQ/Kaana/internal/contract"

// SourceReviewedPrivateAutoApproval is the separately reviewed internal Alia
// classifier authority. It does not authorize Mention, ordinary/public serving,
// or the original one-use 3.6 operation. No environment or wire activation.
// Source and evidence both expire at the exact reviewed 2026-10-05T22:51:12Z.
func SourceReviewedPrivateAutoApproval() *contract.PrivateAutoSourceApproval {
	return &contract.PrivateAutoSourceApproval{
		PrivateAutoAuthority: contract.PrivateAutoAuthority{
			Purpose:                   "private_auto_classifier",
			ClassifierVersion:         "jev-auto-v1",
			ApprovalID:                "alia-private-auto-internal-20261005-01",
			ApprovalVersion:           1,
			ExpiresAt:                 "2026-10-05T22:51:12Z",
			EconomicPolicyVersion:     "oxy-inference-economics/2026-10-04.2",
			EconomicRelationshipID:    "alia-kaana",
			DeploymentID:              "dep_openrouter_typesafe_jev_1_13_private_auto_2026_10_05",
			Provider:                  "openrouter",
			KeyID:                     "b8090dce-82f2-4077-9fc1-fd831a53ca27",
			ModelReference:            "typesafe/jev-1.13@2026-09-17",
			UpstreamModelID:           "typesafe/jev-1.13-20260917",
			PriceVersionID:            "jev_scoped_price_20261004_01",
			ProviderRateCardVersionID: "rc_openrouter_jev_private_auto_2026_10_05",
			ProviderSourceVersion:     "openrouter-api/2026-10-04/typesafe/jev-1.13-20260917/556fab0c5da201c07d4eeafd32b48250fb3fa297b69ed0f9e62a7225ca8511ba",
			MaxCostUSD:                "0.001000000000",
			Principal: contract.PrivateAutoPrincipal{
				AccountID:     "01a0369b-1222-712f-8df6-f8ffeb78ccc2",
				ApplicationID: "6a2f851751b784a86fd0e934",
				CredentialID:  "wl_d50a0056191d0448024900b4",
				Environment:   "production",
				Lane:          "service_token",
			},
			Policy: contract.RoutingPolicyReference{
				RoutingPolicyID: "platform-internal-default",
				PolicyVersion:   1,
			},
			Regions: []contract.Region{},
		},
		Review: contract.PrivateAutoReview{
			InternalUseAllowed:         true,
			InternalUseEvidenceRef:     "root-review:private-auto-variable-root-applicability:sha256:b1d8ea881100d1e1b0753afde2630b3cbb5727d1fb3dd43d3d48047651bd1610",
			LegalReviewEvidenceRef:     "root-review:private-auto-variable-root-applicability:sha256:b1d8ea881100d1e1b0753afde2630b3cbb5727d1fb3dd43d3d48047651bd1610",
			PrivacyEvidenceRef:         "source-evidence:typesafe-privacy.html:sha256:93012e61403f9f7f7536e94914497777f791ab51b017e08bbb5e24bc06cd31c7",
			ZDREvidenceRef:             "source-evidence:zdr-endpoints.json:sha256:cfbf208c4b723c537272d774d2955bb957c35d4d2bfc2c54a383875126aae1b9",
			EvidenceExpiresAt:          "2026-10-05T22:51:12Z",
			CommercialUseAllowed:       false,
			RetainsPayloads:            false,
			RetentionDays:              0,
			TrainsOnCustomerData:       false,
			ZeroDataRetentionAvailable: true,
		},
		Limits: contract.PrivateAutoLimits{
			TimeoutMs:               1000,
			MaxStateBytes:           8192,
			MaxControlledInputBytes: 8192,
		},
	}
}
