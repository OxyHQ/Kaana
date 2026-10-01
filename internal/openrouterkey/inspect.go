// Package openrouterkey reads OpenRouter's description of one existing key,
// GET https://openrouter.ai/api/v1/key, to prove which organization owns it.
//
// It performs no inference. The endpoint is a constant: no
// flag, environment variable or caller can point the key at another origin.
// The response is parsed strictly and only an allow-listed projection leaves
// this package; no identifier, label or body text is ever reported.
//
// What a match proves is the organization (and optionally the workspace) that
// OpenRouter says owns the key at the moment of the request. It proves nothing
// about credits, grant restrictions, purchased balance, auto top-up, or the
// key's future state, and it is no inference budget guarantee.
//
// Schema: https://openrouter.ai/openapi.json, operation getCurrentKey.
package openrouterkey

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/provider"
)

// Endpoint is the only URL this package requests.
const Endpoint = "https://openrouter.ai/api/v1/key"

const (
	// Timeout bounds the whole exchange, body included.
	Timeout = 10 * time.Second
	// MaxResponseBytes bounds the response body read.
	MaxResponseBytes   = 64 << 10
	maxIdentifierBytes = 256
)

var (
	ErrInvalidExpectation  = errors.New("openrouter key inspection: expected identifiers must be 1 to 256 visible ASCII bytes")
	ErrInvalidCredential   = errors.New("openrouter key inspection: credential is not a valid header value")
	ErrRequestFailed       = errors.New("openrouter key inspection: request failed")
	ErrTimeout             = errors.New("openrouter key inspection: request timed out")
	ErrRedirectRefused     = errors.New("openrouter key inspection: redirect refused")
	ErrCredentialRejected  = errors.New("openrouter key inspection: OpenRouter rejected the credential")
	ErrUnexpectedStatus    = errors.New("openrouter key inspection: unexpected response status")
	ErrResponseTooLarge    = errors.New("openrouter key inspection: response exceeds 64 KiB")
	ErrMalformedResponse   = errors.New("openrouter key inspection: response is malformed")
	ErrExpectationNotMet   = errors.New("openrouter key inspection: key ownership does not match the expectation")
	errMalformedNotJSON    = fmt.Errorf("%w: not application/json", ErrMalformedResponse)
	errMalformedDuplicate  = fmt.Errorf("%w: duplicate or case-variant field", ErrMalformedResponse)
	errMalformedStructure  = fmt.Errorf("%w: invalid JSON structure", ErrMalformedResponse)
	errMalformedField      = fmt.Errorf("%w: a known field has an invalid value", ErrMalformedResponse)
	errMalformedMissing    = fmt.Errorf("%w: a required field is missing", ErrMalformedResponse)
	errMalformedIdentifier = fmt.Errorf("%w: an identifier is empty, too long or not visible ASCII", ErrMalformedResponse)
)

// Expectation is the non-secret ownership the operator expects.
type Expectation struct {
	OrganizationID string
	// WorkspaceID is optional; empty means it is not checked or reported.
	WorkspaceID string
}

// Validate refuses identifiers that could not have come from OpenRouter.
func (e Expectation) Validate() error {
	if !validIdentifier(e.OrganizationID) {
		return ErrInvalidExpectation
	}
	if e.WorkspaceID != "" && !validIdentifier(e.WorkspaceID) {
		return ErrInvalidExpectation
	}
	return nil
}

// OwnershipEvidence says what OpenRouter reported, without the identifier.
type OwnershipEvidence string

const (
	// EvidenceMatches: the field is a string equal to the expected id.
	EvidenceMatches OwnershipEvidence = "matches"
	// EvidenceDiffers: the field is a string naming another id.
	EvidenceDiffers OwnershipEvidence = "differs"
	// EvidenceUnknown: absent or null supplies no binding evidence.
	EvidenceUnknown OwnershipEvidence = "bindingNotDemonstrated"
)

// LimitReset is the allow-listed reset cadence; any other string is
// "unrecognized" and never echoed.
type LimitReset string

const (
	LimitResetNone         LimitReset = "none"
	LimitResetDaily        LimitReset = "daily"
	LimitResetWeekly       LimitReset = "weekly"
	LimitResetMonthly      LimitReset = "monthly"
	LimitResetUnrecognized LimitReset = "unrecognized"
)

// Report is the complete output. Every field is allow-listed; nothing from the
// response that is not named here leaves the package.
type Report struct {
	OrganizationMatchesExpected bool              `json:"organizationMatchesExpected"`
	OrganizationEvidence        OwnershipEvidence `json:"organizationEvidence"`
	WorkspaceMatchesExpected    *bool             `json:"workspaceMatchesExpected,omitempty"`
	WorkspaceEvidence           OwnershipEvidence `json:"workspaceEvidence,omitempty"`
	LimitUSD                    *float64          `json:"limitUsd"`
	LimitRemainingUSD           *float64          `json:"limitRemainingUsd"`
	LimitReset                  LimitReset        `json:"limitReset"`
	UsageUSD                    float64           `json:"usageUsd"`
	UsageDailyUSD               float64           `json:"usageDailyUsd"`
	UsageWeeklyUSD              float64           `json:"usageWeeklyUsd"`
	UsageMonthlyUSD             float64           `json:"usageMonthlyUsd"`
	IsFreeTier                  bool              `json:"isFreeTier"`
	ExpiresAt                   *time.Time        `json:"expiresAt"`
}

// Verdict fails closed: nil only when the organization, and the workspace
// when one was expected, positively match.
func (r Report) Verdict() error {
	if !r.OrganizationMatchesExpected {
		return ErrExpectationNotMet
	}
	if r.WorkspaceMatchesExpected != nil && !*r.WorkspaceMatchesExpected {
		return ErrExpectationNotMet
	}
	return nil
}

// Inspector sends the one request. The transport is injectable for tests; the
// URL, method, redirect policy and timeout are not.
type Inspector struct {
	client *http.Client
}

// New builds an inspector over transport. A nil transport is refused.
func New(transport http.RoundTripper) (*Inspector, error) {
	if transport == nil {
		return nil, errors.New("openrouter key inspection: transport is required")
	}
	return &Inspector{
		client: provider.RefuseRedirects(&http.Client{Transport: transport, Timeout: Timeout}),
	}, nil
}

// NewProduction builds an inspector whose transport ignores proxy environment
// variables, so the environment cannot route the key through another host.
func NewProduction() *Inspector {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true // No reused-connection replay of this GET.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true) // HTTP/2 may implicitly retry a fresh GET.
	transport.Protocols.SetHTTP2(false)
	transport.ForceAttemptHTTP2 = false
	inspector, _ := New(transport)
	return inspector
}

// Inspect sends GET Endpoint once with secret as the bearer credential. The
// secret is borrowed: it is not retained, and the caller clears it.
//
// Errors are fixed sentinels. A transport error, response header or body never
// becomes error text, because any of them may echo the Authorization header.
func (i *Inspector) Inspect(ctx context.Context, secret []byte, expected Expectation) (Report, error) {
	if err := expected.Validate(); err != nil {
		return Report{}, err
	}
	if provider.ValidateCustomerCredential(secret) != nil {
		return Report{}, ErrInvalidCredential
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, Endpoint, nil)
	if err != nil {
		return Report{}, ErrRequestFailed
	}
	// net/http needs a string header value; it cannot be cleared and is
	// dropped with the request when this function returns.
	request.Header.Set("Authorization", "Bearer "+string(secret))
	request.Header.Set("Accept", "application/json")

	response, err := i.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Report{}, ErrTimeout
		}
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return Report{}, ErrTimeout
		}
		return Report{}, ErrRequestFailed
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return Report{}, ErrRedirectRefused
	case response.StatusCode == http.StatusUnauthorized:
		return Report{}, ErrCredentialRejected
	case response.StatusCode != http.StatusOK:
		return Report{}, ErrUnexpectedStatus
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Report{}, errMalformedNotJSON
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	defer clear(body)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Report{}, ErrTimeout
		}
		return Report{}, ErrRequestFailed
	}
	if len(body) > MaxResponseBytes {
		return Report{}, ErrResponseTooLarge
	}
	key, err := parse(body)
	if err != nil {
		return Report{}, err
	}
	return key.report(expected), nil
}

type nullableString struct {
	present bool
	null    bool
	value   string
}

type nullableNumber struct {
	present bool
	value   *float64
}

type keyData struct {
	organizationID nullableString
	workspaceID    nullableString
	limit          nullableNumber
	limitRemaining nullableNumber
	limitReset     nullableString
	expiresAt      nullableString
	usage          *float64
	usageDaily     *float64
	usageWeekly    *float64
	usageMonthly   *float64
	isFreeTier     *bool
}

func (k keyData) report(expected Expectation) Report {
	report := Report{
		OrganizationEvidence: evidence(k.organizationID, expected.OrganizationID),
		LimitUSD:             k.limit.value,
		LimitRemainingUSD:    k.limitRemaining.value,
		LimitReset:           limitReset(k.limitReset),
		UsageUSD:             *k.usage,
		UsageDailyUSD:        *k.usageDaily,
		UsageWeeklyUSD:       *k.usageWeekly,
		UsageMonthlyUSD:      *k.usageMonthly,
		IsFreeTier:           *k.isFreeTier,
	}
	report.OrganizationMatchesExpected = report.OrganizationEvidence == EvidenceMatches
	if expected.WorkspaceID != "" {
		report.WorkspaceEvidence = evidence(k.workspaceID, expected.WorkspaceID)
		matches := report.WorkspaceEvidence == EvidenceMatches
		report.WorkspaceMatchesExpected = &matches
	}
	if k.expiresAt.present && !k.expiresAt.null {
		// parse already proved this is RFC 3339.
		expires, _ := time.Parse(time.RFC3339Nano, k.expiresAt.value)
		expires = expires.UTC()
		report.ExpiresAt = &expires
	}
	return report
}

func evidence(field nullableString, expected string) OwnershipEvidence {
	switch {
	case !field.present || field.null:
		return EvidenceUnknown
	case field.value == expected:
		return EvidenceMatches
	default:
		return EvidenceDiffers
	}
}

func limitReset(field nullableString) LimitReset {
	if field.null {
		return LimitResetNone
	}
	switch LimitReset(field.value) {
	case LimitResetDaily, LimitResetWeekly, LimitResetMonthly:
		return LimitReset(field.value)
	default:
		return LimitResetUnrecognized
	}
}

// knownFields are the data fields this package interprets. Every other field
// is skipped (after syntax validation) and never reported.
var knownFields = []string{
	"organization_id", "workspace_id", "limit", "limit_remaining", "limit_reset",
	"expires_at", "usage", "usage_daily", "usage_weekly", "usage_monthly", "is_free_tier",
}

// parse reads {"data":{...}} token by token so a duplicate key is refused
// rather than silently resolved to its last occurrence, as encoding/json does.
func parse(body []byte) (keyData, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var key keyData
	sawData := false
	if err := expectDelim(decoder, '{'); err != nil {
		return keyData{}, err
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		name, err := objectKey(decoder, seen, []string{"data"})
		if err != nil {
			return keyData{}, err
		}
		if name != "data" {
			if err := skipValue(decoder); err != nil {
				return keyData{}, err
			}
			continue
		}
		sawData = true
		if key, err = parseData(decoder); err != nil {
			return keyData{}, err
		}
	}
	if err := expectDelim(decoder, '}'); err != nil {
		return keyData{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return keyData{}, errMalformedStructure
	}
	if !sawData {
		return keyData{}, errMalformedMissing
	}
	return key, nil
}

func parseData(decoder *json.Decoder) (keyData, error) {
	var key keyData
	if err := expectDelim(decoder, '{'); err != nil {
		return keyData{}, err
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		name, err := objectKey(decoder, seen, knownFields)
		if err != nil {
			return keyData{}, err
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return keyData{}, errMalformedStructure
		}
		switch name {
		case "organization_id":
			key.organizationID, err = identifierField(raw)
		case "workspace_id":
			key.workspaceID, err = identifierField(raw)
		case "limit":
			key.limit, err = nullableNumberField(raw)
		case "limit_remaining":
			key.limitRemaining, err = nullableNumberField(raw)
		case "limit_reset":
			key.limitReset, err = stringField(raw)
		case "expires_at":
			key.expiresAt, err = timestampField(raw)
		case "usage":
			key.usage, err = numberField(raw)
		case "usage_daily":
			key.usageDaily, err = numberField(raw)
		case "usage_weekly":
			key.usageWeekly, err = numberField(raw)
		case "usage_monthly":
			key.usageMonthly, err = numberField(raw)
		case "is_free_tier":
			key.isFreeTier, err = boolField(raw)
		}
		if err != nil {
			return keyData{}, err
		}
	}
	if err := expectDelim(decoder, '}'); err != nil {
		return keyData{}, err
	}
	// organization_id and workspace_id are reported as absent, not refused: a
	// missing ownership field is a distinct answer from a different owner.
	if !key.limit.present || !key.limitRemaining.present || !key.limitReset.present ||
		key.usage == nil || key.usageDaily == nil || key.usageWeekly == nil || key.usageMonthly == nil ||
		key.isFreeTier == nil {
		return keyData{}, errMalformedMissing
	}
	return key, nil
}

func objectKey(decoder *json.Decoder, seen map[string]struct{}, known []string) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", errMalformedStructure
	}
	name, ok := token.(string)
	if !ok {
		return "", errMalformedStructure
	}
	if _, duplicate := seen[name]; duplicate {
		return "", errMalformedDuplicate
	}
	seen[name] = struct{}{}
	for _, field := range known {
		if name != field && strings.EqualFold(name, field) {
			return "", errMalformedDuplicate
		}
	}
	return name, nil
}

func expectDelim(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return errMalformedStructure
	}
	if delim, ok := token.(json.Delim); !ok || delim != want {
		return errMalformedStructure
	}
	return nil
}

func skipValue(decoder *json.Decoder) error {
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return errMalformedStructure
	}
	return nil
}

func isNull(raw json.RawMessage) bool { return string(raw) == "null" }

func stringField(raw json.RawMessage) (nullableString, error) {
	if isNull(raw) {
		return nullableString{present: true, null: true}, nil
	}
	if len(raw) == 0 || raw[0] != '"' {
		return nullableString{}, errMalformedField
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nullableString{}, errMalformedField
	}
	return nullableString{present: true, value: value}, nil
}

func identifierField(raw json.RawMessage) (nullableString, error) {
	field, err := stringField(raw)
	if err != nil {
		return nullableString{}, err
	}
	if !field.null && !validIdentifier(field.value) {
		return nullableString{}, errMalformedIdentifier
	}
	return field, nil
}

func timestampField(raw json.RawMessage) (nullableString, error) {
	field, err := stringField(raw)
	if err != nil || field.null {
		return field, err
	}
	if _, err := time.Parse(time.RFC3339Nano, field.value); err != nil {
		return nullableString{}, errMalformedField
	}
	return field, nil
}

// numberField accepts a finite, non-negative JSON number. JSON cannot spell
// NaN or Infinity; an out-of-range literal such as 1e999 is refused here. Any
// minus sign is refused, so -0 cannot be echoed as a negative zero.
func numberField(raw json.RawMessage) (*float64, error) {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return nil, errMalformedField
	}
	value, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil, errMalformedField
	}
	return &value, nil
}

func nullableNumberField(raw json.RawMessage) (nullableNumber, error) {
	if isNull(raw) {
		return nullableNumber{present: true}, nil
	}
	value, err := numberField(raw)
	if err != nil {
		return nullableNumber{}, err
	}
	return nullableNumber{present: true, value: value}, nil
}

func boolField(raw json.RawMessage) (*bool, error) {
	switch string(raw) {
	case "true":
		value := true
		return &value, nil
	case "false":
		value := false
		return &value, nil
	default:
		return nil, errMalformedField
	}
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > maxIdentifierBytes {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] <= ' ' || value[index] > '~' {
			return false
		}
	}
	return true
}
