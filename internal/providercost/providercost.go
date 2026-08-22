// Package providercost measures what a request cost Pensara upstream.
//
// This is the one place in the repository that holds an amount of money, and it
// is deliberately not the contract's. ADR 0006 gives Pensara upstream provider
// cost and gives Oxy every customer-facing amount; `internal/contract` has no
// money type at all and must not acquire one. The number here answers "what
// will the provider invoice us for this request", never "what is this customer
// charged" — those are different questions with different owners, and the
// moment one type answers both, Pensara has started a second ledger.
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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"

	"github.com/OxyHQ/Pensara/internal/contract"
)

// Scale is the number of decimal places an Amount carries: an amount is
// expressed in 1e-12 of the currency's major unit.
//
// It matches the published contract's money scale on purpose. Pensara never
// exchanges money with Oxy, but an operator reconciling a provider invoice
// against the ledger's revenue is comparing these two numbers by hand, and two
// different scales is how that comparison goes wrong by a factor of a thousand.
const Scale = 12

// Money is an amount in one currency, in units of 1e-12.
//
// Integer rather than floating point: provider rates are quoted per million
// tokens and a request's cost is a sum of thousands of them, which is precisely
// the arithmetic that accumulates float error.
type Money struct {
	Currency string
	Amount   int64
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

// currencyPattern is the shape of an ISO 4217 code. Pensara does not hold a
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

// Cards is the loaded rate table.
//
// A nil *Cards is a supported state and means cost measurement is not
// configured: every measurement then reports itself unpriced rather than zero.
type Cards struct {
	byDeployment map[contract.DeploymentID]Card
}

type cardFile struct {
	RateCards []Card `json:"rateCards"`
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
	if len(parsed.RateCards) == 0 {
		return nil, fmt.Errorf("providercost: no rate cards declared; omit the file instead of shipping an empty one")
	}

	cards := &Cards{byDeployment: make(map[contract.DeploymentID]Card, len(parsed.RateCards))}
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
	// Priced is false when the deployment has no rate card at all. It is a
	// distinct state from a zero cost, which is what a card pricing everything
	// at zero would legitimately produce.
	Priced bool
	// UnpricedUnits names units that were measured and not priced. They travel
	// with the amount so that an incomplete cost cannot be mistaken for a
	// complete one further downstream.
	UnpricedUnits []contract.UsageUnit
}

// Complete reports whether every measured unit was priced.
func (m Measurement) Complete() bool { return m.Priced && len(m.UnpricedUnits) == 0 }

// Measure prices one attempt's units.
func (c *Cards) Measure(deployment contract.DeploymentID, units []contract.UsageQuantity) Measurement {
	if c == nil {
		return Measurement{}
	}
	card, found := c.byDeployment[deployment]
	if !found {
		return Measurement{}
	}

	rates := make(map[contract.UsageUnit]int64, len(card.Rates))
	for _, rate := range card.Rates {
		rates[rate.Unit] = rate.AmountPerUnit
	}

	measurement := Measurement{Priced: true, Cost: Money{Currency: card.Currency}}
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
	DeploymentID contract.DeploymentID
	Provider     contract.ProviderSlug
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
	// Pensara has no rate for.
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
		measurement := c.Measure(attempt.DeploymentID, attempt.Units)
		record.Attempts = append(record.Attempts, AttemptCost{AttemptUsage: attempt, Measurement: measurement})
		if !measurement.Complete() {
			record.Complete = false
		}
		if measurement.Priced {
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

// LogValue renders a record for the operator log. It names the request, the
// deployments and the amounts, and nothing about what the request contained.
func (r Record) LogValue() slog.Value {
	amounts := make([]string, 0, len(r.Totals))
	for _, total := range r.Totals {
		amounts = append(amounts, total.String())
	}
	unpriced := make([]string, 0)
	for _, attempt := range r.Attempts {
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
		slog.Bool("complete", r.Complete),
		slog.Any("unpriced", unpriced),
	)
}
