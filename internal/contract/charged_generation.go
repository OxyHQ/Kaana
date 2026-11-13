package contract

import (
	"fmt"
	"regexp"
	"unicode/utf16"
)

// ChargedGenerationRecord is the existing public commercial receipt shape.
// It is distinct from units-only metering; no charged result may be labelled
// metered_usage/not_charged. Its fields are checked against the canonical Zod
// schema by the same descriptor and wire fixture gates as the other Go types.
type ChargedGenerationRecord struct {
	SchemaVersion   int             `json:"schemaVersion"`
	Kind            string          `json:"kind"`
	RequestID       RequestID       `json:"requestId"`
	GenerationID    *GenerationID   `json:"generationId,omitempty"`
	ApplicationID   ApplicationID   `json:"applicationId"`
	CredentialID    CredentialID    `json:"credentialId"`
	DelegatedUserID *UserID         `json:"delegatedUserId,omitempty"`
	Environment     Environment     `json:"environment"`
	Outcome         RequestOutcome  `json:"outcome"`
	UsageSource     UsageSource     `json:"usageSource"`
	Units           []UsageQuantity `json:"units"`
	Model           ModelReference  `json:"model"`
	Labels          RequestLabels   `json:"labels,omitempty"`
	SettledAt       Timestamp       `json:"settledAt"`
	ReceiptID       string          `json:"receiptId"`
	PriceSnapshot   PriceSnapshot   `json:"priceSnapshot"`
	BilledAmount    ExactDecimal    `json:"billedAmount"`
	Currency        CurrencyCode    `json:"currency"`
	PlatformFeeOnly bool            `json:"platformFeeOnly"`
}
type UnitPrice struct {
	Unit     UsageUnit    `json:"unit"`
	Amount   ExactDecimal `json:"amount"`
	Per      int64        `json:"per"`
	Currency CurrencyCode `json:"currency"`
}
type PriceSnapshot struct {
	PriceVersionID string       `json:"priceVersionId"`
	Currency       CurrencyCode `json:"currency"`
	UnitPrices     []UnitPrice  `json:"unitPrices"`
}

var receiptDecimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,17})(?:\.[0-9]{1,12})?$`)
var receiptCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func receiptBounded(value string, min, max int) bool {
	n := len(utf16.Encode([]rune(value)))
	return n >= min && n <= max
}

func (r ChargedGenerationRecord) Validate() error {
	invalid := func(field string) error { return fmt.Errorf("contract: invalid charged generation %s", field) }
	if r.SchemaVersion != 1 || r.Kind != "charge" {
		return invalid("kind/version")
	}
	if !receiptBounded(string(r.RequestID), 1, 128) || !receiptBounded(string(r.ApplicationID), 1, 64) || !receiptBounded(string(r.CredentialID), 1, 64) || !receiptBounded(r.ReceiptID, 1, 128) {
		return invalid("identity")
	}
	if r.GenerationID != nil && !receiptBounded(string(*r.GenerationID), 1, 128) {
		return invalid("generationId")
	}
	if r.DelegatedUserID != nil && !receiptBounded(string(*r.DelegatedUserID), 1, 64) {
		return invalid("delegatedUserId")
	}
	if !isMember(r.Environment, environmentValues) || !isMember(r.Outcome, requestOutcomeValues) || !isMember(r.UsageSource, usageSourceValues) || !r.Model.Valid() {
		return invalid("outcome/model")
	}
	if _, err := r.SettledAt.Time(); err != nil {
		return invalid("settledAt")
	}
	if r.Units == nil {
		return invalid("units")
	}
	seen := map[UsageUnit]bool{}
	for _, u := range r.Units {
		if !isMember(u.Unit, usageUnitValues) || u.Quantity < 0 || u.Quantity > 9007199254740991 || seen[u.Unit] {
			return invalid("units")
		}
		seen[u.Unit] = true
	}
	if len(r.Labels) > 16 {
		return invalid("labels")
	}
	for _, v := range r.Labels {
		if !receiptBounded(v, 0, 256) {
			return invalid("labels")
		}
	}
	if !receiptDecimalPattern.MatchString(string(r.BilledAmount)) || !receiptCurrencyPattern.MatchString(string(r.Currency)) {
		return invalid("money")
	}
	if r.PriceSnapshot.Currency != r.Currency || !receiptBounded(r.PriceSnapshot.PriceVersionID, 1, 128) || len(r.PriceSnapshot.UnitPrices) == 0 {
		return invalid("priceSnapshot")
	}
	for _, p := range r.PriceSnapshot.UnitPrices {
		if !isMember(p.Unit, usageUnitValues) || !receiptDecimalPattern.MatchString(string(p.Amount)) || p.Per < 1 || p.Per > 9007199254740991 || !receiptCurrencyPattern.MatchString(string(p.Currency)) {
			return invalid("unitPrice")
		}
	}
	return nil
}
