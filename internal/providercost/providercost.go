// Package providercost measures what a request cost Kaana upstream.
//
// This is the one place in the repository that holds an amount of money, and it
// is deliberately not the contract's. ADR 0006 gives Kaana upstream provider
// cost and gives Oxy every customer-facing amount; `internal/contract` has no
// money type at all and must not acquire one. The number here answers "what
// will the provider invoice us for this request", never "what is this customer
// charged" — those are different questions with different owners, and the
// moment one type answers both, Kaana has started a second ledger.
//
// # Why it cannot reach a customer
//
// Nothing in this package appears in any produced contract shape. A cost is
// carried out of the executor as a Go value on the execution result, which is
// never marshalled to the wire: the stream events, the usage report and the
// error body have no field it could occupy, and the descriptor gate fails on
// any field added to them that the contract does not have. What consumes a
// measurement is the operator log. `TestUpstreamCostNeverReachesTheCustomer`
// asserts the emitted bytes over a costed request contain no amount, with a
// control proving a non-zero cost was actually measured.
//
// # Why an unknown cost is not a zero cost
//
// A deployment with no rate card, or a unit the card does not price, yields a
// measurement that says so. Summing an unknown as zero produces a reconciliation
// that looks complete and is quietly short by exactly the traffic nobody priced,
// which is the failure this package's shape exists to make impossible: the
// unpriced units travel with the number.
package providercost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Scale is the number of decimal places an Amount carries: an amount is
// expressed in 1e-12 of the currency's major unit.
//
// It matches the published contract's money scale on purpose. Kaana never
// exchanges money with Oxy, but an operator reconciling a provider invoice
// against the ledger's revenue is comparing these two numbers by hand, and two
// different scales is how that comparison goes wrong by a factor of a thousand.
const Scale = 12

const maxPersistenceBatchEvents = 64

// Money is an amount in one currency, in units of 1e-12.
//
// Integer rather than floating point: provider rates are quoted per million
// tokens and a request's cost is a sum of thousands of them, which is precisely
// the arithmetic that accumulates float error.
type Money struct {
	Currency string
	Amount   int64
}

// ParseDecimal parses a provider-reported decimal amount without passing
// through floating point. Providers use fewer than Scale decimal places in
// practice; accepting more would silently round an invoice fact.
func ParseDecimal(currency, value string) (Money, error) {
	if !currencyPattern.MatchString(currency) {
		return Money{}, fmt.Errorf("providercost: %q is not a currency code", currency)
	}
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		return Money{}, fmt.Errorf("providercost: %q is not a non-negative decimal amount", value)
	}
	whole, fraction, found := strings.Cut(value, ".")
	if !found {
		fraction = ""
	}
	if whole == "" || len(fraction) > Scale || strings.Contains(fraction, ".") {
		return Money{}, fmt.Errorf("providercost: %q is not a decimal with at most %d places", value, Scale)
	}
	for _, part := range []string{whole, fraction} {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return Money{}, fmt.Errorf("providercost: %q is not a decimal amount", value)
			}
		}
	}
	var amount int64
	for _, digit := range whole + fraction + strings.Repeat("0", Scale-len(fraction)) {
		if amount > (int64(^uint64(0)>>1)-int64(digit-'0'))/10 {
			return Money{}, fmt.Errorf("providercost: %q exceeds the supported amount", value)
		}
		amount = amount*10 + int64(digit-'0')
	}
	return Money{Currency: currency, Amount: amount}, nil
}

// String renders an amount for an operator log.
func (m Money) String() string {
	if m.Currency == "" {
		return "unmeasured"
	}
	whole := m.Amount / 1_000_000_000_000
	fraction := m.Amount % 1_000_000_000_000
	if fraction < 0 {
		fraction = -fraction
	}
	return fmt.Sprintf("%s %d.%012d", m.Currency, whole, fraction)
}

// currencyPattern is the shape of an ISO 4217 code. Kaana does not hold a
// currency table: it never converts between currencies, so the only thing it
// can usefully check is that an operator has not written a provider's name
// where a currency belongs.
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

/* -------------------------------------------------------------------------- */
/*  Rate cards                                                                */
/* -------------------------------------------------------------------------- */

// Rate is what one unit costs upstream.
type Rate struct {
	Unit contract.UsageUnit `json:"unit"`
	// AmountPerUnit is in the same 1e-12 scale as Money. A provider quoting
	// $2.50 per million input tokens is 2_500_000 here.
	AmountPerUnit int64 `json:"amountPerUnit"`
}

// Card prices one deployment.
type Card struct {
	DeploymentID contract.DeploymentID `json:"deploymentId"`
	Currency     string                `json:"currency"`
	Rates        []Rate                `json:"rates"`
}

type RateCardSource string

const (
	RateCardProviderAPI           RateCardSource = "provider_api"
	RateCardProviderDocumentation RateCardSource = "provider_documentation"
	RateCardOperator              RateCardSource = "operator"
)

// Cards is the loaded rate table.
//
// A nil *Cards is a supported state and means cost measurement is not
// configured: every measurement then reports itself unpriced rather than zero.
type Cards struct {
	byDeployment map[contract.DeploymentID]Card
	versionID    string
	effectiveAt  time.Time
	expiresAt    *time.Time
}

type cardFile struct {
	SchemaVersion int            `json:"schemaVersion"`
	VersionID     string         `json:"rateCardVersionId"`
	Source        RateCardSource `json:"source"`
	SourceVersion string         `json:"sourceVersion"`
	ObservedAt    time.Time      `json:"observedAt"`
	EffectiveAt   time.Time      `json:"effectiveAt"`
	ExpiresAt     *time.Time     `json:"expiresAt,omitempty"`
	RateCards     []Card         `json:"rateCards"`
}

// Load reads rate cards from a JSON file.
func Load(path string) (*Cards, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("providercost: reading %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse builds a rate table, refusing anything that would produce a plausible
// wrong number.
func Parse(raw []byte) (*Cards, error) {
	var parsed cardFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("providercost: %w", err)
	}
	if parsed.SchemaVersion != 1 || strings.TrimSpace(parsed.VersionID) != parsed.VersionID || parsed.VersionID == "" ||
		strings.TrimSpace(parsed.SourceVersion) != parsed.SourceVersion || parsed.SourceVersion == "" ||
		parsed.ObservedAt.IsZero() || parsed.EffectiveAt.IsZero() {
		return nil, fmt.Errorf("providercost: rate-card observation identity is incomplete")
	}
	if parsed.Source != RateCardProviderAPI && parsed.Source != RateCardProviderDocumentation && parsed.Source != RateCardOperator {
		return nil, fmt.Errorf("providercost: rate-card observation source %q is invalid", parsed.Source)
	}
	if parsed.ExpiresAt != nil && !parsed.ExpiresAt.After(parsed.EffectiveAt) {
		return nil, fmt.Errorf("providercost: rate-card observation has an invalid validity window")
	}
	if len(parsed.RateCards) == 0 {
		return nil, fmt.Errorf("providercost: no rate cards declared; omit the file instead of shipping an empty one")
	}

	cards := &Cards{
		byDeployment: make(map[contract.DeploymentID]Card, len(parsed.RateCards)),
		versionID:    parsed.VersionID, effectiveAt: parsed.EffectiveAt, expiresAt: parsed.ExpiresAt,
	}
	for _, card := range parsed.RateCards {
		switch {
		case card.DeploymentID == "":
			return nil, fmt.Errorf("providercost: a rate card names no deployment")
		case !currencyPattern.MatchString(card.Currency):
			return nil, fmt.Errorf("providercost: %s prices in %q, which is not a currency code", card.DeploymentID, card.Currency)
		case len(card.Rates) == 0:
			return nil, fmt.Errorf("providercost: %s declares no rates, which would price every request at zero", card.DeploymentID)
		}
		if _, duplicate := cards.byDeployment[card.DeploymentID]; duplicate {
			return nil, fmt.Errorf("providercost: two rate cards price %s", card.DeploymentID)
		}

		seen := make(map[contract.UsageUnit]struct{}, len(card.Rates))
		for _, rate := range card.Rates {
			if !rate.Unit.Valid() {
				return nil, fmt.Errorf("providercost: %s prices %q, which is not a usage unit", card.DeploymentID, rate.Unit)
			}
			if rate.AmountPerUnit < 0 {
				return nil, fmt.Errorf("providercost: %s prices %s negatively", card.DeploymentID, rate.Unit)
			}
			if _, duplicate := seen[rate.Unit]; duplicate {
				return nil, fmt.Errorf("providercost: %s prices %s twice", card.DeploymentID, rate.Unit)
			}
			seen[rate.Unit] = struct{}{}
		}
		cards.byDeployment[card.DeploymentID] = card
	}
	return cards, nil
}

// Priced reports whether a deployment has a rate card.
func (c *Cards) Priced(deployment contract.DeploymentID) bool {
	if c == nil {
		return false
	}
	_, found := c.byDeployment[deployment]
	return found
}

/* -------------------------------------------------------------------------- */
/*  Measurement                                                               */
/* -------------------------------------------------------------------------- */

// Measurement is what one upstream attempt cost.
type Measurement struct {
	Cost Money
	// Source says whether Cost came from the provider's exact receipt or from
	// Kaana's versioned rate card. Unknown carries no amount and can never be
	// interpreted as a free request.
	Source Source
	// RateCardVersionID binds an estimate to the immutable observation used to
	// calculate it. Exact and unknown costs carry no rate-card identity.
	RateCardVersionID string
	// ProviderBilledCustomer is true for BYOK: the provider charged the
	// customer's own account, so the attempt is completely accounted for but is
	// not an expense Kaana may add to its provider-cost totals.
	ProviderBilledCustomer bool
	// Priced is false when the deployment has no rate card at all. It is a
	// distinct state from a zero cost, which is what a card pricing everything
	// at zero would legitimately produce.
	Priced bool
	// UnpricedUnits names units that were measured and not priced. They travel
	// with the amount so that an incomplete cost cannot be mistaken for a
	// complete one further downstream.
	UnpricedUnits []contract.UsageUnit
}

// Source is the provenance of an upstream-cost measurement.
type Source string

const (
	SourceUnknown          Source = "unknown"
	SourceRateCard         Source = "rate_card"
	SourceProviderReported Source = "provider_reported"
)

// Complete reports whether every measured unit was priced.
func (m Measurement) Complete() bool {
	return m.ProviderBilledCustomer || (m.Priced && len(m.UnpricedUnits) == 0)
}

// Measure prices one attempt's units.
func (c *Cards) Measure(deployment contract.DeploymentID, units []contract.UsageQuantity) Measurement {
	return c.measureAt(deployment, units, time.Now())
}

func (c *Cards) measureAt(deployment contract.DeploymentID, units []contract.UsageQuantity, at time.Time) Measurement {
	if c == nil {
		return Measurement{Source: SourceUnknown}
	}
	if at.IsZero() {
		at = time.Now()
	}
	if at.Before(c.effectiveAt) || (c.expiresAt != nil && !at.Before(*c.expiresAt)) {
		return Measurement{Source: SourceUnknown}
	}
	card, found := c.byDeployment[deployment]
	if !found {
		return Measurement{Source: SourceUnknown}
	}

	rates := make(map[contract.UsageUnit]int64, len(card.Rates))
	for _, rate := range card.Rates {
		rates[rate.Unit] = rate.AmountPerUnit
	}

	measurement := Measurement{Priced: true, Source: SourceRateCard, RateCardVersionID: c.versionID, Cost: Money{Currency: card.Currency}}
	for _, quantity := range units {
		rate, priced := rates[quantity.Unit]
		if !priced {
			measurement.UnpricedUnits = append(measurement.UnpricedUnits, quantity.Unit)
			continue
		}
		measurement.Cost.Amount += rate * int64(quantity.Quantity)
	}
	sort.Slice(measurement.UnpricedUnits, func(a, b int) bool {
		return measurement.UnpricedUnits[a] < measurement.UnpricedUnits[b]
	})
	return measurement
}

/* -------------------------------------------------------------------------- */
/*  One request, several attempts                                             */
/* -------------------------------------------------------------------------- */

// AttemptUsage is what one upstream attempt consumed.
//
// A failover request has several. The units of an attempt that failed are NOT
// on the customer's usage report — they never received that output — but the
// provider will invoice for them all the same, so they are here. That asymmetry
// is the reason this measurement exists separately from the usage report rather
// than as a field on it.
type AttemptUsage struct {
	AttemptIndex   int
	DeploymentID   contract.DeploymentID
	Provider       contract.ProviderSlug
	ModelReference contract.ModelReference
	OccurredAt     time.Time
	// ProviderBilledCustomer marks a request-scoped BYOK attempt. Its usage is
	// retained for reconciliation, but its upstream amount belongs to the
	// customer's provider account rather than Kaana's cost ledger.
	ProviderBilledCustomer bool
	// KeyID names the pool key this attempt spent, and KeyClass what the
	// operator said it costs. A deployment is served by a POOL, so the
	// deployment id cannot answer which credential ran out — and the credential
	// is the thing a budget is kept against.
	KeyID    string
	KeyClass string
	// ProviderReportedCost is the exact amount the upstream says it billed for
	// this attempt. It outranks a rate-card calculation but never enters a
	// customer response.
	ProviderReportedCost *Money
	// Served marks the attempt whose output reached the customer. At most one
	// attempt per request is served.
	Served bool
	Units  []contract.UsageQuantity
}

// AttemptCost is one attempt, priced.
type AttemptCost struct {
	AttemptUsage
	Measurement
}

// Record is the whole request's upstream cost.
type Record struct {
	RequestID contract.RequestID
	Attempts  []AttemptCost
	// Totals is one amount per currency, sorted. Several currencies in one
	// request is unusual and legitimate — two providers serving one model line
	// can invoice differently — and adding them together would be a conversion
	// Kaana has no rate for.
	Totals []Money
	// Complete is false if any attempt was unpriced or carried an unpriced
	// unit. A reconciliation that treats an incomplete record as complete is
	// short by exactly the traffic nobody priced.
	Complete bool
}

// MeasureRequest prices every attempt of one request.
func (c *Cards) MeasureRequest(requestID contract.RequestID, attempts []AttemptUsage) Record {
	record := Record{RequestID: requestID, Complete: true}
	totals := make(map[string]int64)

	for _, attempt := range attempts {
		measurement := Measurement{ProviderBilledCustomer: attempt.ProviderBilledCustomer, Source: SourceUnknown}
		if !attempt.ProviderBilledCustomer {
			if attempt.ProviderReportedCost != nil {
				measurement = Measurement{Cost: *attempt.ProviderReportedCost, Priced: true, Source: SourceProviderReported}
			} else {
				measurement = c.measureAt(attempt.DeploymentID, attempt.Units, attempt.OccurredAt)
			}
		}
		record.Attempts = append(record.Attempts, AttemptCost{AttemptUsage: attempt, Measurement: measurement})
		if !measurement.Complete() {
			record.Complete = false
		}
		if measurement.Priced && !measurement.ProviderBilledCustomer {
			totals[measurement.Cost.Currency] += measurement.Cost.Amount
		}
	}
	if len(attempts) == 0 {
		record.Complete = false
	}

	for currency, amount := range totals {
		record.Totals = append(record.Totals, Money{Currency: currency, Amount: amount})
	}
	sort.Slice(record.Totals, func(a, b int) bool { return record.Totals[a].Currency < record.Totals[b].Currency })
	return record
}

// Event is one operator-only upstream-cost fact ready for durable storage.
// Customer BYOK attempts are absent: their provider account, and therefore
// their expense, belongs to the customer rather than Kaana.
type Event struct {
	RequestID         contract.RequestID
	AttemptIndex      int
	Provider          contract.ProviderSlug
	KeyID             string
	DeploymentID      contract.DeploymentID
	ModelReference    contract.ModelReference
	Cost              Money
	Source            Source
	RateCardVersionID string
	Complete          bool
	Served            bool
	OccurredAt        time.Time
}

// Writer is the narrow persistence authority used by Recorder.
type Writer interface {
	WriteProviderCostEvent(context.Context, Event) error
}

// BatchWriter persists one request's attempts in a single atomic operation.
// Production storage implements this interface; the single-event Writer keeps
// the package usable with narrow in-memory measurement sinks.
type BatchWriter interface {
	WriteProviderCostEvents(context.Context, []Event) error
}

// Recorder persists each platform-funded attempt in a measured request.
type Recorder struct {
	writer Writer
}

// NewRecorder builds the operator-cost persistence boundary.
func NewRecorder(writer Writer) (*Recorder, error) {
	if writer == nil {
		return nil, fmt.Errorf("providercost: no event writer")
	}
	return &Recorder{writer: writer}, nil
}

// Record validates every platform-funded attempt before writing. Production
// storage receives one atomic batch and is retried as a whole; no prefix of a
// multi-attempt request can become durable by itself.
func (r *Recorder) Record(ctx context.Context, record Record) error {
	if len(record.Attempts) > maxPersistenceBatchEvents {
		return fmt.Errorf("providercost: request %s has more than %d attempts", record.RequestID, maxPersistenceBatchEvents)
	}
	events := make([]Event, 0, len(record.Attempts))
	seenAttemptIndexes := make(map[int]struct{}, len(record.Attempts))
	for _, attempt := range record.Attempts {
		// No key identity means no credential was leased and no upstream call
		// was made (for example, an entirely retired pool). It is an execution
		// refusal rather than a provider-cost event.
		if attempt.AttemptUsage.ProviderBilledCustomer || attempt.KeyID == "" {
			continue
		}
		event := Event{
			RequestID: record.RequestID, AttemptIndex: attempt.AttemptIndex,
			Provider: attempt.Provider, KeyID: attempt.KeyID,
			DeploymentID: attempt.DeploymentID, ModelReference: attempt.ModelReference,
			Cost: attempt.Cost, Source: attempt.Source, RateCardVersionID: attempt.RateCardVersionID, Complete: attempt.Complete(),
			Served: attempt.Served, OccurredAt: attempt.OccurredAt,
		}
		if err := validateEvent(event); err != nil {
			return err
		}
		if _, duplicate := seenAttemptIndexes[event.AttemptIndex]; duplicate {
			return fmt.Errorf("providercost: request %s repeats attempt index %d", record.RequestID, event.AttemptIndex)
		}
		seenAttemptIndexes[event.AttemptIndex] = struct{}{}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil
	}
	if writer, supported := r.writer.(BatchWriter); supported {
		var err error
		for retryDelay := 25 * time.Millisecond; ; retryDelay *= 4 {
			err = writer.WriteProviderCostEvents(ctx, events)
			if err == nil {
				return nil
			}
			if retryDelay > 100*time.Millisecond {
				break
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("providercost: recording request %s atomically: %w", record.RequestID, ctx.Err())
			case <-timer.C:
			}
		}
		return fmt.Errorf("providercost: recording request %s atomically after retries: %w", record.RequestID, err)
	}
	for _, event := range events {
		if err := r.writer.WriteProviderCostEvent(ctx, event); err != nil {
			return fmt.Errorf("providercost: recording request %s attempt %d: %w", record.RequestID, event.AttemptIndex, err)
		}
	}
	return nil
}

func validateEvent(event Event) error {
	if event.RequestID == "" || event.AttemptIndex < 0 || event.Provider == "" || event.KeyID == "" ||
		event.DeploymentID == "" || event.ModelReference == "" || event.OccurredAt.IsZero() {
		return fmt.Errorf("providercost: event identity is incomplete")
	}
	switch event.Source {
	case SourceProviderReported, SourceRateCard:
		if !currencyPattern.MatchString(event.Cost.Currency) || event.Cost.Amount < 0 {
			return fmt.Errorf("providercost: a priced event has invalid money")
		}
		if event.Source == SourceRateCard && (event.RateCardVersionID == "" || len(event.RateCardVersionID) > 256) {
			return fmt.Errorf("providercost: a rate-card event has no exact rate-card version")
		}
		if event.Source == SourceProviderReported && event.RateCardVersionID != "" {
			return fmt.Errorf("providercost: a provider-reported event carries a rate-card version")
		}
	case SourceUnknown:
		if event.Cost.Currency != "" || event.Cost.Amount != 0 || event.Complete || event.RateCardVersionID != "" {
			return fmt.Errorf("providercost: an unknown event carries a cost or claims completeness")
		}
	default:
		return fmt.Errorf("providercost: event has unknown source %q", event.Source)
	}
	return nil
}

// LogValue renders a record for the operator log. It names the request, the
// deployments and the amounts, and nothing about what the request contained.
func (r Record) LogValue() slog.Value {
	amounts := make([]string, 0, len(r.Totals))
	for _, total := range r.Totals {
		amounts = append(amounts, total.String())
	}
	unpriced := make([]string, 0)
	customerBilled := 0
	for _, attempt := range r.Attempts {
		if attempt.Measurement.ProviderBilledCustomer {
			customerBilled++
			continue
		}
		if !attempt.Priced {
			unpriced = append(unpriced, string(attempt.DeploymentID)+":no-rate-card")
			continue
		}
		for _, unit := range attempt.UnpricedUnits {
			unpriced = append(unpriced, string(attempt.DeploymentID)+":"+string(unit))
		}
	}
	return slog.GroupValue(
		slog.Any("totals", amounts),
		slog.Int("attempts", len(r.Attempts)),
		slog.Int("customerBilledAttempts", customerBilled),
		slog.Bool("complete", r.Complete),
		slog.Any("unpriced", unpriced),
	)
}
