package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// The grant: how Oxy decides who may spend, for every billable request Kaana
// serves on https://kaana.ai/v1 (Kaana ADR 0002, decisions 3 and 4;
// `@kaana.ai/contracts/internal`, src/internal/grant.ts).
//
// Kaana asks POST https://api.oxy.so/internal/kaana/v1/authorizations with the
// caller's credential verbatim and content-free facts, and Oxy answers with a
// grant signed under GrantSignatureDomain. Kaana verifies the signature, the
// execution window and the body hash, and executes inside the grant.

// GrantSignatureDomain is the Ed25519 signature domain of a grant. Same keys as
// the envelope (KAANA_EDGE_PUBLIC_KEYS).
const GrantSignatureDomain = "oxy-kaana-grant:v1"

// SettlementSignatureDomain is the domain of a settlement, signed with Kaana's
// own settlement key.
const SettlementSignatureDomain = "kaana-oxy-settlement:v1"

// GrantExecutionWindow is how long after issuedAt Kaana may START executing a
// grant.
const GrantExecutionWindow = 60 * time.Second

// GrantSchemaVersion, AuthorizationSchemaVersion and SettlementSchemaVersion
// are the per-shape versions of the grant exchange.
const (
	GrantSchemaVersion         = 1
	AuthorizationSchemaVersion = 1
	SettlementSchemaVersion    = 1
)

// Authorization kinds: the discriminator of authorizationRequestSchema.
const (
	AuthorizationKindInference = "inference"
	AuthorizationKindCatalogue = "catalogue.read"
	AuthorizationKindReceipt   = "receipt.read"
)

// sha256HexPattern is the grammar of every hash the grant exchange binds.
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256Hex reports whether value is 64 lowercase hex characters.
func ValidSHA256Hex(value string) bool { return sha256HexPattern.MatchString(value) }

/* -------------------------------------------------------------------------- */
/*  Vocabulary                                                                */
/* -------------------------------------------------------------------------- */

// GrantOperation is what a request asks to do.
type GrantOperation string

const (
	GrantResponses           GrantOperation = "responses"
	GrantChatCompletions     GrantOperation = "chat_completions"
	GrantEmbeddings          GrantOperation = "embeddings"
	GrantAudioSpeech         GrantOperation = "audio_speech"
	GrantAudioTranscriptions GrantOperation = "audio_transcriptions"
	GrantImagesGenerations   GrantOperation = "images_generations"
	GrantDecisions           GrantOperation = "decisions"
	GrantRealtimeSession     GrantOperation = "realtime_session"
)

var grantOperationValues = []GrantOperation{
	GrantResponses, GrantChatCompletions, GrantEmbeddings, GrantAudioSpeech,
	GrantAudioTranscriptions, GrantImagesGenerations, GrantDecisions, GrantRealtimeSession,
}

// Valid reports whether the operation is one the contract names.
func (o GrantOperation) Valid() bool { return isMember(o, grantOperationValues) }

// PowerLevel is a power level a caller may target instead of a model.
type PowerLevel string

const (
	PowerAuto    PowerLevel = "auto"
	PowerInstant PowerLevel = "instant"
	PowerMedium  PowerLevel = "medium"
	PowerHigh    PowerLevel = "high"
	PowerXHigh   PowerLevel = "xhigh"
	PowerPro     PowerLevel = "pro"
	PowerUltra   PowerLevel = "ultra"
)

var powerLevelValues = []PowerLevel{PowerAuto, PowerInstant, PowerMedium, PowerHigh, PowerXHigh, PowerPro, PowerUltra}

// Valid reports whether the level is one the contract names.
func (l PowerLevel) Valid() bool { return isMember(l, powerLevelValues) }

// ParsePowerLevel reads a level, reporting whether value is one.
func ParsePowerLevel(value string) (PowerLevel, bool) {
	level := PowerLevel(value)
	return level, level.Valid()
}

// GrantLane is how the caller authenticated, which decides who pays.
type GrantLane string

const (
	LaneMachineCredential GrantLane = "machine_credential"
	LaneServiceToken      GrantLane = "service_token"
	LaneProductSession    GrantLane = "product_session"
)

var grantLaneValues = []GrantLane{LaneMachineCredential, LaneServiceToken, LaneProductSession}

// EconomicTreatment is how Oxy charges a grant.
type EconomicTreatment string

const (
	TreatmentCommercial      EconomicTreatment = "commercial"
	TreatmentInternalMetered EconomicTreatment = "internal_metered"
	TreatmentPlatformFeeOnly EconomicTreatment = "platform_fee_only"
)

var economicTreatmentValues = []EconomicTreatment{TreatmentCommercial, TreatmentInternalMetered, TreatmentPlatformFeeOnly}

// ModelRevisionLabel is an immutable revision label of one model line.
type ModelRevisionLabel string

// ExactDecimal is an exact non-negative decimal string. Kaana carries Oxy's
// amounts verbatim and never computes with one.
type ExactDecimal string

// CurrencyCode is an ISO 4217 alpha-3 code.
type CurrencyCode string

// Money is an exact amount Oxy put in a grant to be SHOWN. Kaana never spends,
// quotes or derives one (docs/cost.md): it is a value it relays.
type Money struct {
	Amount   ExactDecimal `json:"amount"`
	Currency CurrencyCode `json:"currency"`
}

// RequestLabels are the caller's cost-attribution labels.
type RequestLabels map[string]string

/* -------------------------------------------------------------------------- */
/*  The authorization request Kaana makes                                     */
/* -------------------------------------------------------------------------- */

// GrantTargetKind discriminates GrantTarget.
type GrantTargetKind string

const (
	GrantTargetModel GrantTargetKind = "model"
	GrantTargetLevel GrantTargetKind = "level"
)

var grantTargetKindValues = []GrantTargetKind{GrantTargetModel, GrantTargetLevel}

// GrantTarget is what the caller asked to be served: a model line, optionally
// pinned to a revision, or a power level.
type GrantTarget struct {
	Kind      GrantTargetKind     `json:"kind"`
	ModelLine *ModelID            `json:"modelLine,omitempty"`
	Revision  *ModelRevisionLabel `json:"revision,omitempty"`
	Level     *PowerLevel         `json:"level,omitempty"`
}

// ModelTarget is the target naming one model line (and revision, if pinned).
func ModelTarget(reference ModelReference) GrantTarget {
	line := reference.ModelID()
	target := GrantTarget{Kind: GrantTargetModel, ModelLine: &line}
	if _, revision, pinned := strings.Cut(string(reference), "@"); pinned {
		label := ModelRevisionLabel(revision)
		target.Revision = &label
	}
	return target
}

// LevelTarget is the target naming one power level.
func LevelTarget(level PowerLevel) GrantTarget {
	return GrantTarget{Kind: GrantTargetLevel, Level: &level}
}

// Validate applies the discriminated union's rules.
func (t GrantTarget) Validate() error {
	switch t.Kind {
	case GrantTargetModel:
		if t.ModelLine == nil || t.Level != nil || !modelIDPattern.MatchString(string(*t.ModelLine)) {
			return errors.New("contract: a model target names one model line and no level")
		}
	case GrantTargetLevel:
		if t.Level == nil || !t.Level.Valid() || t.ModelLine != nil || t.Revision != nil {
			return errors.New("contract: a level target names one power level and no model")
		}
	default:
		return fmt.Errorf("contract: unknown grant target kind %q", t.Kind)
	}
	return nil
}

// Reference is the model reference a model target names.
func (t GrantTarget) Reference() ModelReference {
	if t.ModelLine == nil {
		return ""
	}
	if t.Revision != nil {
		return ModelReference(string(*t.ModelLine) + "@" + string(*t.Revision))
	}
	return ModelReference(*t.ModelLine)
}

// GrantRequestFacts are the content-free facts Oxy sizes a hold from.
type GrantRequestFacts struct {
	EstimatedInputTokens   *int                 `json:"estimatedInputTokens,omitempty"`
	MaxOutputTokens        *int                 `json:"maxOutputTokens,omitempty"`
	InputCharacters        *int                 `json:"inputCharacters,omitempty"`
	AudioInputMilliseconds *int                 `json:"audioInputMilliseconds,omitempty"`
	EmbeddingInputs        *int                 `json:"embeddingInputs,omitempty"`
	Images                 *int                 `json:"images,omitempty"`
	DeclaredTools          int                  `json:"declaredTools"`
	Stream                 bool                 `json:"stream"`
	RealtimeSessionKind    *RealtimeSessionKind `json:"realtimeSessionKind,omitempty"`
}

// AuthorizationClient is the request metadata an authorization carries.
type AuthorizationClient struct {
	Endpoint        string        `json:"endpoint"`
	ClientRequestID *string       `json:"clientRequestId,omitempty"`
	ReceivedAt      Timestamp     `json:"receivedAt"`
	Labels          RequestLabels `json:"labels,omitempty"`
}

// InferenceAuthorizationRequest asks Oxy for a grant for one inference
// operation. Credential is the caller's bearer, forwarded opaque and never
// stored or logged.
type InferenceAuthorizationRequest struct {
	SchemaVersion        int                 `json:"schemaVersion"`
	Kind                 string              `json:"kind"`
	Credential           string              `json:"credential"`
	RequestID            RequestID           `json:"requestId"`
	Operation            GrantOperation      `json:"operation"`
	Target               *GrantTarget        `json:"target,omitempty"`
	Facts                GrantRequestFacts   `json:"facts"`
	IdempotencyKey       *IdempotencyKey     `json:"idempotencyKey,omitempty"`
	OnBehalfOfUserID     *UserID             `json:"onBehalfOfUserId,omitempty"`
	BodySHA256           string              `json:"bodySha256"`
	CanonicalInputSHA256 *string             `json:"canonicalInputSha256,omitempty"`
	Client               AuthorizationClient `json:"client"`
}

// CatalogueAuthorizationRequest asks what a credential may see of the
// catalogue. No reservation.
type CatalogueAuthorizationRequest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Kind          string    `json:"kind"`
	Credential    string    `json:"credential"`
	RequestID     RequestID `json:"requestId"`
}

// ReceiptAuthorizationRequest asks Oxy for a generation record.
type ReceiptAuthorizationRequest struct {
	SchemaVersion    int             `json:"schemaVersion"`
	Kind             string          `json:"kind"`
	Credential       string          `json:"credential"`
	RequestID        RequestID       `json:"requestId"`
	GenerationID     *GenerationID   `json:"generationId,omitempty"`
	IdempotencyKey   *IdempotencyKey `json:"idempotencyKey,omitempty"`
	OnBehalfOfUserID *UserID         `json:"onBehalfOfUserId,omitempty"`
}

/* -------------------------------------------------------------------------- */
/*  The grant Oxy signs                                                       */
/* -------------------------------------------------------------------------- */

// GrantPrincipal is who pays and who called. BillingAccountID is the owner of
// the authenticated application, unresolved: Oxy's ledger resolves the
// billable ancestor.
type GrantPrincipal struct {
	BillingAccountID AccountID     `json:"billingAccountId"`
	ApplicationID    ApplicationID `json:"applicationId"`
	CredentialID     CredentialID  `json:"credentialId"`
	Environment      Environment   `json:"environment"`
	Scopes           []Scope       `json:"scopes"`
	Lane             GrantLane     `json:"lane"`
}

// AdmittedModelLine is a model line Oxy admits for a step, with the price it
// settles at.
type AdmittedModelLine struct {
	ModelLine      ModelID `json:"modelLine"`
	PriceVersionID string  `json:"priceVersionId"`
}

// GrantStepLimits bound one step: the Console policy, already applied by Oxy,
// as values Kaana filters by.
type GrantStepLimits struct {
	MaxPricePerRequest             *Money              `json:"maxPricePerRequest,omitempty"`
	MaxFundingTier                 *FundingTier        `json:"maxFundingTier,omitempty"`
	AdmittedModelLines             []AdmittedModelLine `json:"admittedModelLines"`
	ProviderAllowlist              []ProviderSlug      `json:"providerAllowlist,omitempty"`
	ProviderDenylist               []ProviderSlug      `json:"providerDenylist,omitempty"`
	AllowedRegions                 []Region            `json:"allowedRegions,omitempty"`
	DeniedRegions                  []Region            `json:"deniedRegions,omitempty"`
	RequireZeroDataRetention       bool                `json:"requireZeroDataRetention"`
	ProhibitTrainingOnCustomerData bool                `json:"prohibitTrainingOnCustomerData"`
	AllowedLicenseIDs              []string            `json:"allowedLicenseIds,omitempty"`
	RequireCommercialUseRights     bool                `json:"requireCommercialUseRights"`
	// SameModelFailover is whether a request may move, inside this step,
	// between providers serving the SAME admitted line (contract set 4.4.0).
	// Absent is true: Oxy that predates the member keeps the failover it
	// always had. Read it through SameModelFailoverAllowed.
	SameModelFailover *bool `json:"sameModelFailover,omitempty"`
}

// SameModelFailoverAllowed is SameModelFailover with its default applied.
func (l GrantStepLimits) SameModelFailoverAllowed() bool {
	return l.SameModelFailover == nil || *l.SameModelFailover
}

// GrantStep is one ordered step: a target and its limits.
type GrantStep struct {
	Target GrantTarget     `json:"target"`
	Limits GrantStepLimits `json:"limits"`
}

// GrantExactRouteApproval names the source approval an exact route carries.
type GrantExactRouteApproval string

const (
	ApprovalScopedExecution   GrantExactRouteApproval = "scoped_execution"
	ApprovalPrivateAuto       GrantExactRouteApproval = "private_auto"
	ApprovalMentionClassifier GrantExactRouteApproval = "mention_classifier"
)

var grantExactRouteApprovalValues = []GrantExactRouteApproval{ApprovalScopedExecution, ApprovalPrivateAuto, ApprovalMentionClassifier}

// GrantExactRoute is the exact route of an operation with a source approval.
type GrantExactRoute struct {
	Approval       GrantExactRouteApproval `json:"approval"`
	DeploymentID   DeploymentID            `json:"deploymentId"`
	Provider       ProviderSlug            `json:"provider"`
	KeyID          string                  `json:"keyId"`
	ModelReference ModelReference          `json:"modelReference"`
	PriceVersionID string                  `json:"priceVersionId"`
	InputSHA256    string                  `json:"inputSha256"`
}

// GrantEconomics is how Oxy charges the grant.
type GrantEconomics struct {
	Treatment      EconomicTreatment `json:"treatment"`
	PolicyVersion  string            `json:"policyVersion"`
	RelationshipID *string           `json:"relationshipId,omitempty"`
}

// GrantReservation is the hold a charged grant was sized against.
type GrantReservation struct {
	ReservationID      string  `json:"reservationId"`
	Ceiling            Money   `json:"ceiling"`
	FundingRestriction *string `json:"fundingRestriction,omitempty"`
}

// GrantCapacity is the capacity slot an internal relationship claimed.
type GrantCapacity struct {
	MeteredUsageID string `json:"meteredUsageId"`
}

// GrantClient is the client metadata a grant carries back.
type GrantClient struct {
	Labels RequestLabels `json:"labels,omitempty"`
}

// Grant is the signed body of a successful inference authorization.
type Grant struct {
	SchemaVersion              int                         `json:"schemaVersion"`
	GrantID                    string                      `json:"grantId"`
	RequestID                  RequestID                   `json:"requestId"`
	Operation                  GrantOperation              `json:"operation"`
	IssuedAt                   Timestamp                   `json:"issuedAt"`
	ExecuteBy                  Timestamp                   `json:"executeBy"`
	BodySHA256                 string                      `json:"bodySha256"`
	Principal                  GrantPrincipal              `json:"principal"`
	UserID                     *UserID                     `json:"userId,omitempty"`
	Economics                  GrantEconomics              `json:"economics"`
	Reservation                *GrantReservation           `json:"reservation,omitempty"`
	Capacity                   *GrantCapacity              `json:"capacity,omitempty"`
	Steps                      []GrantStep                 `json:"steps"`
	ExactRoute                 *GrantExactRoute            `json:"exactRoute,omitempty"`
	CustomerProviderCredential *CustomerProviderCredential `json:"customerProviderCredential,omitempty"`
	IdempotencyKey             *IdempotencyKey             `json:"idempotencyKey,omitempty"`
	Client                     GrantClient                 `json:"client"`
	RoutingPolicy              RoutingPolicyReference      `json:"routingPolicy"`
}

// maxGrantSteps bounds the ordered steps of one grant (contract: 1..8).
const maxGrantSteps = 8

// DecodeGrant reads a grant strictly: a signed document whose field this build
// does not understand is refused rather than partly read, because a field
// dropped here would be a limit Kaana silently stopped enforcing.
func DecodeGrant(body []byte) (*Grant, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var grant Grant
	if err := decoder.Decode(&grant); err != nil {
		return nil, fmt.Errorf("contract: the grant is not readable: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("contract: the grant has trailing JSON")
	}
	if err := grant.Validate(); err != nil {
		return nil, err
	}
	return &grant, nil
}

// Validate applies every rule grantSchema states, so a grant Oxy's own parse
// would refuse is refused here too.
func (g *Grant) Validate() error {
	switch {
	case g.SchemaVersion != GrantSchemaVersion:
		return fmt.Errorf("contract: grant schemaVersion %d is not %d", g.SchemaVersion, GrantSchemaVersion)
	case g.GrantID == "" || len(g.GrantID) > 128:
		return errors.New("contract: a grant has an id of 1..128 characters")
	case g.RequestID == "" || len(g.RequestID) > 128:
		return errors.New("contract: a grant names its request")
	case !g.Operation.Valid():
		return fmt.Errorf("contract: unknown grant operation %q", g.Operation)
	case !ValidSHA256Hex(g.BodySHA256):
		return errors.New("contract: a grant binds a lowercase hex SHA-256 body")
	case !isMember(g.Principal.Lane, grantLaneValues):
		return fmt.Errorf("contract: unknown grant lane %q", g.Principal.Lane)
	case !isMember(g.Economics.Treatment, economicTreatmentValues):
		return fmt.Errorf("contract: unknown economic treatment %q", g.Economics.Treatment)
	case g.Economics.PolicyVersion == "":
		return errors.New("contract: a grant names its economic policy version")
	case len(g.Steps) == 0 || len(g.Steps) > maxGrantSteps:
		return fmt.Errorf("contract: a grant has 1..%d steps", maxGrantSteps)
	case g.Principal.BillingAccountID == "" || g.Principal.ApplicationID == "" || g.Principal.CredentialID == "":
		return errors.New("contract: a grant names its paying principal")
	case !isMember(g.Principal.Environment, environmentValues):
		return errors.New("contract: a grant names its environment")
	}
	issued, err := time.Parse(time.RFC3339Nano, string(g.IssuedAt))
	if err != nil {
		return errors.New("contract: a grant's issuedAt is not a timestamp")
	}
	executeBy, err := time.Parse(time.RFC3339Nano, string(g.ExecuteBy))
	if err != nil {
		return errors.New("contract: a grant's executeBy is not a timestamp")
	}
	if !executeBy.After(issued) || executeBy.Sub(issued) > GrantExecutionWindow {
		return errors.New("contract: a grant is executable for at most 60 seconds after it is issued")
	}
	metered := g.Economics.Treatment == TreatmentInternalMetered
	if metered && g.Reservation != nil {
		return errors.New("contract: an internally metered grant holds nothing")
	}
	if !metered && g.Reservation == nil {
		return errors.New("contract: a charged grant carries the hold it was sized against")
	}
	if g.Reservation != nil {
		if g.Reservation.ReservationID == "" || len(g.Reservation.ReservationID) > 128 {
			return errors.New("contract: a reservation has an id of 1..128 characters")
		}
		if r := g.Reservation.FundingRestriction; r != nil && *r != "promotional-only" {
			return fmt.Errorf("contract: unknown funding restriction %q", *r)
		}
	}
	byok := g.CustomerProviderCredential != nil
	if byok != (g.Economics.Treatment == TreatmentPlatformFeeOnly) {
		return errors.New("contract: a customer provider credential is exactly the platform-fee-only treatment")
	}
	if byok {
		if err := g.CustomerProviderCredential.validate(); err != nil {
			return err
		}
	}
	for index, step := range g.Steps {
		if err := step.validate(); err != nil {
			return fmt.Errorf("contract: steps[%d]: %w", index, err)
		}
		if byok && step.Limits.MaxFundingTier != nil {
			return fmt.Errorf("contract: steps[%d]: a funding ceiling bounds platform keys; a customer credential is billed to the customer", index)
		}
	}
	if route := g.ExactRoute; route != nil {
		line, revision, pinned := strings.Cut(string(route.ModelReference), "@")
		step := g.Steps[0]
		if !pinned || len(g.Steps) != 1 || step.Target.Kind != GrantTargetModel ||
			string(*step.Target.ModelLine) != line || step.Target.Revision == nil ||
			string(*step.Target.Revision) != revision || byok {
			return errors.New("contract: an exact route is one platform route for the one pinned model the grant serves")
		}
		admitted := false
		for _, candidate := range step.Limits.AdmittedModelLines {
			if string(candidate.ModelLine) == line && candidate.PriceVersionID == route.PriceVersionID {
				admitted = true
			}
		}
		if !admitted {
			return errors.New("contract: the exact route settles at its admitted price")
		}
		if !isMember(route.Approval, grantExactRouteApprovalValues) || !ValidSHA256Hex(route.InputSHA256) {
			return errors.New("contract: the exact route names its approval and input hash")
		}
	}
	return nil
}

func (s GrantStep) validate() error {
	if err := s.Target.Validate(); err != nil {
		return err
	}
	limits := s.Limits
	if len(limits.AdmittedModelLines) == 0 {
		return errors.New("a step admits at least one model line")
	}
	seen := make(map[ModelID]struct{}, len(limits.AdmittedModelLines))
	for _, line := range limits.AdmittedModelLines {
		if !modelIDPattern.MatchString(string(line.ModelLine)) || line.PriceVersionID == "" || len(line.PriceVersionID) > 128 {
			return errors.New("an admitted model line names a model id and its price version")
		}
		if _, duplicate := seen[line.ModelLine]; duplicate {
			return errors.New("each model line is admitted once, at one price")
		}
		seen[line.ModelLine] = struct{}{}
	}
	if s.Target.Kind == GrantTargetModel {
		if _, admitted := seen[*s.Target.ModelLine]; !admitted {
			return errors.New("a step that names a model line admits it")
		}
	}
	if limits.MaxFundingTier != nil && !limits.MaxFundingTier.Valid() {
		return fmt.Errorf("unknown funding tier %q", *limits.MaxFundingTier)
	}
	denied := make(map[ProviderSlug]struct{}, len(limits.ProviderDenylist))
	for _, provider := range limits.ProviderDenylist {
		denied[provider] = struct{}{}
	}
	for _, provider := range limits.ProviderAllowlist {
		if _, both := denied[provider]; both {
			return errors.New("a provider cannot be both allowed and denied")
		}
	}
	deniedRegions := make(map[Region]struct{}, len(limits.DeniedRegions))
	for _, region := range limits.DeniedRegions {
		deniedRegions[region] = struct{}{}
	}
	for _, region := range limits.AllowedRegions {
		if _, both := deniedRegions[region]; both {
			return errors.New("a region cannot be both allowed and denied")
		}
	}
	return nil
}

// Executable reports whether the grant may still START executing at now.
func (g *Grant) Executable(now time.Time) bool {
	executeBy, err := time.Parse(time.RFC3339Nano, string(g.ExecuteBy))
	return err == nil && !now.After(executeBy)
}

// Attribution is the attribution every usage report of this grant carries:
// who pays and who called, exactly as Oxy resolved them.
func (g *Grant) Attribution() Attribution {
	return Attribution{
		Principal: AuthenticatedPrincipal{
			Billing:         BillingPrincipal{AccountID: g.Principal.BillingAccountID},
			ApplicationID:   g.Principal.ApplicationID,
			CredentialID:    g.Principal.CredentialID,
			Environment:     g.Principal.Environment,
			InferenceScopes: append([]Scope(nil), g.Principal.Scopes...),
		},
		UserID:    g.UserID,
		RequestID: g.RequestID,
	}
}

/* -------------------------------------------------------------------------- */
/*  Catalogue and receipt access                                              */
/* -------------------------------------------------------------------------- */

// CatalogueAudience is which catalogue a credential sees.
type CatalogueAudience string

const (
	AudiencePublic           CatalogueAudience = "public"
	AudiencePlatformInternal CatalogueAudience = "platform_internal"
)

var catalogueAudienceValues = []CatalogueAudience{AudiencePublic, AudiencePlatformInternal}

// CatalogueAccess is Oxy's answer to a catalogue.read authorization.
type CatalogueAccess struct {
	SchemaVersion int               `json:"schemaVersion"`
	Kind          string            `json:"kind"`
	Audience      CatalogueAudience `json:"audience"`
	AllowedLevels []PowerLevel      `json:"allowedLevels,omitempty"`
	ExpiresAt     Timestamp         `json:"expiresAt"`
}

// Validate applies catalogueAccessSchema.
func (c *CatalogueAccess) Validate() error {
	if c.SchemaVersion != AuthorizationSchemaVersion || c.Kind != AuthorizationKindCatalogue || !isMember(c.Audience, catalogueAudienceValues) {
		return errors.New("contract: the catalogue access is not a catalogue.read answer")
	}
	for _, level := range c.AllowedLevels {
		if !level.Valid() {
			return fmt.Errorf("contract: unknown power level %q", level)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, string(c.ExpiresAt)); err != nil {
		return errors.New("contract: the catalogue access has no expiry")
	}
	return nil
}

// ReceiptAccess is Oxy's answer to a receipt.read authorization. The record is
// Oxy's (it carries Oxy's amounts); Kaana relays it to the caller verbatim and
// never reads an amount from it.
type ReceiptAccess struct {
	SchemaVersion int             `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	Record        json.RawMessage `json:"record"`
}

/* -------------------------------------------------------------------------- */
/*  Settlement                                                                */
/* -------------------------------------------------------------------------- */

// Settlement is one granted request's settlement, as Kaana's outbox delivers
// it: units, never money, idempotent on GrantID.
type Settlement struct {
	SchemaVersion  int         `json:"schemaVersion"`
	SettlementID   string      `json:"settlementId"`
	GrantID        string      `json:"grantId"`
	RequestID      RequestID   `json:"requestId"`
	ReservationID  *string     `json:"reservationId,omitempty"`
	MeteredUsageID *string     `json:"meteredUsageId,omitempty"`
	ServedStep     *int        `json:"servedStep,omitempty"`
	Report         UsageReport `json:"report"`
	CreatedAt      Timestamp   `json:"createdAt"`
}

// Validate applies settlementSchema, including the usage report's own rules.
func (s *Settlement) Validate() error {
	switch {
	case s.SchemaVersion != SettlementSchemaVersion:
		return errors.New("contract: settlement schemaVersion is not 1")
	case s.SettlementID == "" || len(s.SettlementID) > 128 || s.GrantID == "" || len(s.GrantID) > 128:
		return errors.New("contract: a settlement names itself and its grant")
	case s.Report.RequestID != s.RequestID:
		return errors.New("contract: the report settles this request")
	case s.Report.Outcome == OutcomeCompleted && s.ServedStep == nil:
		return errors.New("contract: a completed request was served by a step of its grant")
	case s.ServedStep != nil && (*s.ServedStep < 0 || *s.ServedStep >= maxGrantSteps):
		return errors.New("contract: servedStep is a position in the grant")
	}
	return s.Report.Validate()
}

// SettlementStatus is Oxy's verdict on a delivered settlement.
type SettlementStatus string

const (
	SettlementSettled   SettlementStatus = "settled"
	SettlementDuplicate SettlementStatus = "duplicate"
)

var settlementStatusValues = []SettlementStatus{SettlementSettled, SettlementDuplicate}

// SettlementAcknowledgement is Oxy's answer to a delivered settlement. A
// redelivery is duplicate, never a second charge.
type SettlementAcknowledgement struct {
	SchemaVersion int              `json:"schemaVersion"`
	SettlementID  string           `json:"settlementId"`
	Status        SettlementStatus `json:"status"`
}

// Validate applies settlementAcknowledgementSchema.
func (a *SettlementAcknowledgement) Validate() error {
	if a.SchemaVersion != SettlementSchemaVersion || a.SettlementID == "" || !isMember(a.Status, settlementStatusValues) {
		return errors.New("contract: the settlement acknowledgement is invalid")
	}
	return nil
}

// SettlementPage is GET /internal/v1/settlements: what Oxy reconciles against.
type SettlementPage struct {
	Data       []Settlement `json:"data"`
	NextCursor *string      `json:"nextCursor,omitempty"`
}
