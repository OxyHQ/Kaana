package credentialstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// MaxAttemptFeedPage is the most attempts one feed read returns.
const MaxAttemptFeedPage = 500

// AttemptFeedCursor is a keyset position in the attempt feed. It is opaque to
// Oxy: Encode and ParseAttemptFeedCursor are its only producers and readers.
type AttemptFeedCursor struct {
	CreatedAt    time.Time
	RequestID    string
	AttemptIndex int
}

const attemptFeedCursorPrefix = "kaf_"

// Encode renders the cursor as an opaque token.
func (c AttemptFeedCursor) Encode() string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.Itoa(c.AttemptIndex) + "|" + c.RequestID
	return attemptFeedCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseAttemptFeedCursor reads a token Encode produced, and refuses anything
// else rather than guessing a position.
func ParseAttemptFeedCursor(token string) (AttemptFeedCursor, error) {
	invalid := errors.New("credential store: attempt feed cursor is invalid")
	encoded, found := strings.CutPrefix(token, attemptFeedCursorPrefix)
	if !found || len(encoded) > 512 {
		return AttemptFeedCursor{}, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return AttemptFeedCursor{}, invalid
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return AttemptFeedCursor{}, invalid
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return AttemptFeedCursor{}, invalid
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil || index < 0 || strconv.Itoa(index) != parts[1] || parts[2] == "" || len(parts[2]) > 256 {
		return AttemptFeedCursor{}, invalid
	}
	return AttemptFeedCursor{CreatedAt: createdAt, RequestID: parts[2], AttemptIndex: index}, nil
}

// AttemptFeedEvent is one upstream attempt as Oxy reads it: exact key, exact
// deployment and model, what it measured, how it ended, and what it cost
// upstream with that cost's provenance. It is an operator projection and never
// part of an inference response.
type AttemptFeedEvent struct {
	Position          string                   `json:"position"`
	RequestID         contract.RequestID       `json:"requestId"`
	AttemptIndex      int                      `json:"attemptIndex"`
	Provider          contract.ProviderSlug    `json:"provider"`
	KeyID             string                   `json:"keyId"`
	KeyClass          string                   `json:"keyClass"`
	DeploymentID      contract.DeploymentID    `json:"deploymentId"`
	ModelReference    contract.ModelReference  `json:"modelReference"`
	Cost              *FeedMoney               `json:"cost"`
	CostSource        string                   `json:"costSource"`
	RateCardVersionID *string                  `json:"rateCardVersionId"`
	CostComplete      bool                     `json:"costComplete"`
	Served            bool                     `json:"served"`
	OccurredAt        time.Time                `json:"occurredAt"`
	Units             []contract.UsageQuantity `json:"units"`
	// Telemetry is null for an attempt recorded before attempts were
	// measured; it is never a zero-filled stand-in.
	Telemetry *FeedAttemptTelemetry `json:"telemetry"`
}

// FeedMoney is an upstream amount in 1e-12 of the currency's major unit, as a
// decimal integer string so no consumer rounds it through a float.
type FeedMoney struct {
	Currency    string `json:"currency"`
	AmountPicos string `json:"amountPicos"`
}

// FeedAttemptTelemetry is one attempt's own measurements.
type FeedAttemptTelemetry struct {
	StartedAt           time.Time `json:"startedAt"`
	LatencyMs           int       `json:"latencyMs"`
	TimeToFirstOutputMs *int      `json:"timeToFirstOutputMs"`
	Outcome             string    `json:"outcome"`
	FailureCode         *string   `json:"failureCode"`
}

// ReadAttemptFeed returns up to limit attempts after the cursor (from the
// start when after is nil), and the cursor to resume from. A page shorter than
// limit means the reader has caught up to the settle window.
func (p *Postgres) ReadAttemptFeed(ctx context.Context, after *AttemptFeedCursor, limit int) ([]AttemptFeedEvent, *AttemptFeedCursor, error) {
	if limit < 1 || limit > MaxAttemptFeedPage {
		return nil, nil, fmt.Errorf("credential store: attempt feed limit must be between 1 and %d", MaxAttemptFeedPage)
	}
	var (
		afterCreatedAt *time.Time
		afterRequestID *string
		afterIndex     *int
	)
	if after != nil {
		afterCreatedAt, afterRequestID, afterIndex = &after.CreatedAt, &after.RequestID, &after.AttemptIndex
	}
	rows, err := p.pool.Query(ctx, `SELECT * FROM kaana_read_provider_attempt_feed($1, $2, $3, $4)`,
		afterCreatedAt, afterRequestID, afterIndex, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("credential store: reading the attempt feed: %w", err)
	}
	defer rows.Close()
	events := make([]AttemptFeedEvent, 0)
	var last *AttemptFeedCursor
	for rows.Next() {
		var (
			event                    AttemptFeedEvent
			createdAt                time.Time
			currency                 *string
			amountPicos              *string
			unitsJSON                []byte
			startedAt                *time.Time
			latencyMs, firstOutputMs *int
			outcome, failureCode     *string
		)
		if err := rows.Scan(&createdAt, &event.RequestID, &event.AttemptIndex, &event.Provider, &event.KeyID,
			&event.KeyClass, &event.DeploymentID, &event.ModelReference, &currency, &amountPicos, &event.CostSource,
			&event.RateCardVersionID, &event.CostComplete, &event.Served, &event.OccurredAt, &unitsJSON, &startedAt,
			&latencyMs, &firstOutputMs, &outcome, &failureCode); err != nil {
			return nil, nil, fmt.Errorf("credential store: reading an attempt feed row: %w", err)
		}
		if currency != nil && amountPicos != nil {
			event.Cost = &FeedMoney{Currency: *currency, AmountPicos: *amountPicos}
		}
		event.Units = make([]contract.UsageQuantity, 0)
		if unitsJSON != nil {
			if err := json.Unmarshal(unitsJSON, &event.Units); err != nil {
				return nil, nil, fmt.Errorf("credential store: decoding attempt units: %w", err)
			}
		}
		if outcome != nil && startedAt != nil && latencyMs != nil {
			event.Telemetry = &FeedAttemptTelemetry{StartedAt: *startedAt, LatencyMs: *latencyMs,
				TimeToFirstOutputMs: firstOutputMs, Outcome: *outcome, FailureCode: failureCode}
		}
		last = &AttemptFeedCursor{CreatedAt: createdAt, RequestID: string(event.RequestID), AttemptIndex: event.AttemptIndex}
		event.Position = last.Encode()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("credential store: reading the attempt feed: %w", err)
	}
	if last == nil {
		last = after
	}
	return events, last, nil
}

// CredentialEconomics is what Oxy may order on for one enabled platform key.
// Description is null for a key nobody has described: Oxy must treat its
// category and commercial-use eligibility as unknown, not as free or allowed.
type CredentialEconomics struct {
	Provider         contract.ProviderSlug         `json:"provider"`
	KeyID            string                        `json:"keyId"`
	KeyClass         string                        `json:"keyClass"`
	Description      *CredentialEconomicsDescribed `json:"description"`
	CapacityEvidence json.RawMessage               `json:"capacityEvidence"`
}

// CredentialEconomicsDescribed is the label-free part of a key's description.
type CredentialEconomicsDescribed struct {
	Revision            int      `json:"revision"`
	CapacityCategory    string   `json:"capacityCategory"`
	Environment         string   `json:"environment"`
	CommercialUse       string   `json:"commercialUse"`
	FundingAccountID    string   `json:"fundingAccountId"`
	AllowedModels       []string `json:"allowedModels"`
	AllowedCapabilities []string `json:"allowedCapabilities"`
}

// ReadCredentialEconomics returns every enabled platform key's economic facts.
func (p *Postgres) ReadCredentialEconomics(ctx context.Context) ([]CredentialEconomics, error) {
	rows, err := p.pool.Query(ctx, `SELECT * FROM kaana_read_provider_credential_economics()`)
	if err != nil {
		return nil, fmt.Errorf("credential store: reading credential economics: %w", err)
	}
	defer rows.Close()
	economics := make([]CredentialEconomics, 0)
	for rows.Next() {
		var (
			row                                        CredentialEconomics
			revision                                   *int
			category, environment, commercial, account *string
			models, capabilities                       []string
			evidence                                   []byte
		)
		if err := rows.Scan(&row.Provider, &row.KeyID, &row.KeyClass, &revision, &category, &environment,
			&commercial, &account, &models, &capabilities, &evidence); err != nil {
			return nil, fmt.Errorf("credential store: reading a credential economics row: %w", err)
		}
		if revision != nil && category != nil && environment != nil && commercial != nil && account != nil {
			row.Description = &CredentialEconomicsDescribed{Revision: *revision, CapacityCategory: *category,
				Environment: *environment, CommercialUse: *commercial, FundingAccountID: *account,
				AllowedModels: models, AllowedCapabilities: capabilities}
		}
		row.CapacityEvidence = evidence
		economics = append(economics, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading credential economics: %w", err)
	}
	return economics, nil
}
