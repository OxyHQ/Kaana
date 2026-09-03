package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validAuthorizedRouteRequest() Request {
	profileID := RoutingProfileID("rpf_01JQZEXACT")
	text := "hello"
	authorized := true
	return Request{
		SchemaVersion: RequestEnvelopeVersion,
		Attribution: Attribution{
			Principal: AuthenticatedPrincipal{
				Billing:         BillingPrincipal{AccountID: "acc_test"},
				ApplicationID:   "app_test",
				CredentialID:    "cred_test",
				Environment:     EnvironmentDevelopment,
				InferenceScopes: []Scope{ScopeInvoke},
			},
			RequestID: "req_test",
		},
		Target:   RoutingTarget{Kind: TargetRoutingProfileID, RoutingProfileID: &profileID},
		Modality: ModalityText,
		Input: Input{
			Format: InputMessages,
			Messages: []Message{{
				Role:    RoleUser,
				Content: []ContentPart{{Type: ContentPartText, Text: &text}},
			}},
		},
		Sampling: SamplingParameters{},
		Client: ClientRequestMetadata{
			APIFormat:  APIFormatResponses,
			Endpoint:   "/v1/responses",
			ReceivedAt: NewTimestamp(time.Now()),
		},
		RoutingPolicy: RoutingPolicyReference{RoutingPolicyID: "rp", PolicyVersion: 1},
		AuthorizedRoutes: []AuthorizedRoute{
			{
				Substitution:   SubstitutionSameModel,
				DeploymentID:   "dep_primary",
				ModelReference: "stub/model@2026-05-01",
				Provider:       "stub",
				Regions:        []Region{"r1"},
			},
			{
				Substitution:       SubstitutionCrossModel,
				DeploymentID:       "dep_alternate",
				ModelReference:     "backup/other@2026-06-01",
				Provider:           "backup",
				Regions:            []Region{"r2"},
				AuthorizedByPolicy: &authorized,
			},
		},
	}
}

func TestRequestEnvelopeVersionTransitionIsNarrow(t *testing.T) {
	legacy := validAuthorizedRouteRequest()
	legacy.SchemaVersion = LegacyRequestEnvelopeVersion
	reference := ModelReference("stub/model@2026-05-01")
	legacy.Target = RoutingTarget{Kind: TargetModel, ModelReference: &reference}
	legacy.AuthorizedRoutes = legacy.AuthorizedRoutes[:1]
	if err := legacy.Validate(); err != nil {
		t.Fatalf("the transitional direct-model envelope was refused: %v", err)
	}

	legacy.Target = RoutingTarget{Kind: TargetRoutingProfileID, RoutingProfileID: pointerTo(RoutingProfileID("rpf_exact"))}
	if err := legacy.Validate(); err == nil || !strings.Contains(err.Error(), "accepts only a direct model target") {
		t.Fatalf("legacy exact-profile target refusal = %v", err)
	}

	var retiredSlug Request
	encoded := []byte(`{"schemaVersion":1,"target":{"kind":"routing_profile","routingProfile":"auto"}}`)
	if err := json.Unmarshal(encoded, &retiredSlug); err != nil {
		if !strings.Contains(err.Error(), "routingProfile is retired") {
			t.Fatalf("the retired v1 slug decode refusal = %v", err)
		}
	} else if err := retiredSlug.Validate(); err == nil {
		t.Fatal("the retired v1 slug target was accepted")
	}

	var smuggledSlug Request
	smuggled := []byte(`{"schemaVersion":1,"target":{"kind":"model","modelReference":"stub/model","routingProfile":"auto"}}`)
	if err := json.Unmarshal(smuggled, &smuggledSlug); err == nil || !strings.Contains(err.Error(), "routingProfile is retired") {
		t.Fatalf("the slug smuggled beside a direct model produced %v", err)
	}

	current := validAuthorizedRouteRequest()
	if err := current.Validate(); err != nil {
		t.Fatalf("the v2 exact-profile target was refused: %v", err)
	}
}

func TestRoutingProfileIDIsAnExactOpaquePrimaryKey(t *testing.T) {
	request := validAuthorizedRouteRequest()
	for name, profileID := range map[string]RoutingProfileID{
		"empty":    "",
		"too long": RoutingProfileID(strings.Repeat("x", 129)),
	} {
		t.Run(name, func(t *testing.T) {
			request := request
			request.Target.RoutingProfileID = &profileID
			if err := request.Validate(); err == nil {
				t.Fatal("the invalid exact routing-profile id was accepted")
			}
		})
	}

	exact := RoutingProfileID("  Case-Sensitive/opaque:id  ")
	request.Target.RoutingProfileID = &exact
	if err := request.Validate(); err != nil {
		t.Fatalf("an opaque id was parsed as a slug instead of preserved exactly: %v", err)
	}
}

func TestAuthorizedRouteRefinementsMatchThePublishedContract(t *testing.T) {
	valid := validAuthorizedRouteRequest()
	if err := valid.Validate(); err != nil {
		t.Fatalf("the valid control was refused: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Request)
		field  string
	}{
		{
			name: "present list is empty",
			mutate: func(request *Request) {
				request.AuthorizedRoutes = []AuthorizedRoute{}
			},
			field: "authorizedRoutes",
		},
		{
			name: "primary claims to be a substitution",
			mutate: func(request *Request) {
				authorized := true
				request.AuthorizedRoutes[0].Substitution = SubstitutionCrossModel
				request.AuthorizedRoutes[0].AuthorizedByPolicy = &authorized
			},
			field: "authorizedRoutes[0].substitution",
		},
		{
			name: "route is unpinned",
			mutate: func(request *Request) {
				request.AuthorizedRoutes[0].ModelReference = "stub/model"
			},
			field: "authorizedRoutes[0]",
		},
		{
			name: "cross model lacks literal authorization",
			mutate: func(request *Request) {
				refused := false
				request.AuthorizedRoutes[1].AuthorizedByPolicy = &refused
			},
			field: "authorizedRoutes[1]",
		},
		{
			name: "same model carries cross-model authorization",
			mutate: func(request *Request) {
				authorized := true
				request.AuthorizedRoutes[0].AuthorizedByPolicy = &authorized
			},
			field: "authorizedRoutes[0]",
		},
		{
			name: "same model label hides another line",
			mutate: func(request *Request) {
				request.AuthorizedRoutes[1].Substitution = SubstitutionSameModel
				request.AuthorizedRoutes[1].AuthorizedByPolicy = nil
			},
			field: "authorizedRoutes[1].substitution",
		},
		{
			name: "cross model label names the primary line",
			mutate: func(request *Request) {
				request.AuthorizedRoutes[1].ModelReference = "stub/model@2026-05-01"
			},
			field: "authorizedRoutes[1].substitution",
		},
		{
			name: "deployment repeats",
			mutate: func(request *Request) {
				request.AuthorizedRoutes[1].DeploymentID = request.AuthorizedRoutes[0].DeploymentID
			},
			field: "authorizedRoutes",
		},
		{
			name: "pinned target primary differs",
			mutate: func(request *Request) {
				reference := ModelReference("stub/different@2026-05-01")
				request.Target = RoutingTarget{Kind: TargetModel, ModelReference: &reference}
				request.AuthorizedRoutes = request.AuthorizedRoutes[:1]
			},
			field: "authorizedRoutes[0].modelReference",
		},
		{
			name: "pinned target includes cross-model route",
			mutate: func(request *Request) {
				reference := ModelReference("stub/model@2026-05-01")
				request.Target = RoutingTarget{Kind: TargetModel, ModelReference: &reference}
			},
			field: "authorizedRoutes[1].substitution",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := validAuthorizedRouteRequest()
			testCase.mutate(&request)
			err := request.Validate()
			if err == nil {
				t.Fatal("the malformed authorized route list was accepted")
			}
			if !strings.Contains(err.Error(), testCase.field) {
				t.Errorf("the refusal %q does not name %q", err, testCase.field)
			}
		})
	}
}

func TestAuthorizedRouteScalarConstraintsAreEnforced(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AuthorizedRoute)
	}{
		{"empty deployment", func(route *AuthorizedRoute) { route.DeploymentID = "" }},
		{"long deployment", func(route *AuthorizedRoute) { route.DeploymentID = DeploymentID(strings.Repeat("x", 129)) }},
		{"invalid provider", func(route *AuthorizedRoute) { route.Provider = "Not Valid" }},
		{"missing regions", func(route *AuthorizedRoute) { route.Regions = nil }},
		{"invalid region", func(route *AuthorizedRoute) { route.Regions = []Region{"Not Valid"} }},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := validAuthorizedRouteRequest()
			testCase.mutate(&request.AuthorizedRoutes[0])
			if err := request.Validate(); err == nil {
				t.Fatal("the malformed authorized route was accepted")
			}
		})
	}
}

func TestAuthorizedRouteAcceptsAnExplicitEmptyUnattestedRegionSet(t *testing.T) {
	request := validAuthorizedRouteRequest()
	request.AuthorizedRoutes[0].Regions = []Region{}
	if err := request.Validate(); err != nil {
		t.Fatalf("an explicit empty unattested region set was refused: %v", err)
	}
}

func TestAuthorizedRouteCustomerCredentialBindingIsExact(t *testing.T) {
	request := validAuthorizedRouteRequest()
	request.AuthorizedRoutes[0].CustomerProviderCredential = &CustomerProviderCredential{
		CredentialHandle:   "kcred_abcdefghijklmnopqrstuvwxyz",
		CredentialRevision: 7,
		OwnerAccountID:     "acc_customer",
		ConnectionID:       "pcx_exact",
		Environment:        EnvironmentProduction,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("the exact customer credential binding was refused: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CustomerProviderCredential)
	}{
		{"handle prefix", func(binding *CustomerProviderCredential) {
			binding.CredentialHandle = "secret_abcdefghijklmnopqrstuvwxyz"
		}},
		{"handle alphabet", func(binding *CustomerProviderCredential) {
			binding.CredentialHandle = "kcred_abcdefghijklmnopqrstuvwxy1"
		}},
		{"zero revision", func(binding *CustomerProviderCredential) { binding.CredentialRevision = 0 }},
		{"unsafe revision", func(binding *CustomerProviderCredential) { binding.CredentialRevision = 1 << 53 }},
		{"empty owner", func(binding *CustomerProviderCredential) { binding.OwnerAccountID = "" }},
		{"long connection", func(binding *CustomerProviderCredential) { binding.ConnectionID = strings.Repeat("x", 129) }},
		{"unknown environment", func(binding *CustomerProviderCredential) { binding.Environment = "live" }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			malformed := request
			binding := *request.AuthorizedRoutes[0].CustomerProviderCredential
			testCase.mutate(&binding)
			malformed.AuthorizedRoutes = append([]AuthorizedRoute(nil), request.AuthorizedRoutes...)
			malformed.AuthorizedRoutes[0].CustomerProviderCredential = &binding
			if err := malformed.Validate(); err == nil || !strings.Contains(err.Error(), "customerProviderCredential") {
				t.Fatalf("the malformed binding was reported as %v", err)
			}
		})
	}
}

func TestAuthorizedRouteRejectsAnExplicitNullCustomerCredentialOnTheWire(t *testing.T) {
	encoded := []byte(`{
		"substitution":"same_model",
		"deploymentId":"dep_primary",
		"modelReference":"stub/model@2026-05-01",
		"provider":"stub",
		"regions":[],
		"customerProviderCredential":null
	}`)

	var route AuthorizedRoute
	if err := json.Unmarshal(encoded, &route); err == nil || !strings.Contains(err.Error(), "customerProviderCredential") {
		t.Fatalf("the explicit null customer binding was reported as %v", err)
	}
}

func TestOnlyAssistantMessagesCarryRefusals(t *testing.T) {
	request := validAuthorizedRouteRequest()
	text := "I cannot help with that"
	request.Input.Messages[0] = Message{
		Role:    RoleAssistant,
		Content: []ContentPart{{Type: ContentPartRefusal, Text: &text}},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("an assistant refusal was rejected: %v", err)
	}

	request.Input.Messages[0].Role = RoleUser
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "only an assistant message carries a refusal") {
		t.Fatalf("a user refusal was reported as %v", err)
	}
}
