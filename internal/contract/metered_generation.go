package contract

// MeteredGenerationRecord is the existing public units-only generation shape.
// Native admissions persist it in Kaana; it is not a commercial zero-price receipt.
type MeteredGenerationRecord struct {
	SchemaVersion   int                   `json:"schemaVersion"`
	Kind            string                `json:"kind"`
	RequestID       RequestID             `json:"requestId"`
	GenerationID    *GenerationID         `json:"generationId,omitempty"`
	ApplicationID   ApplicationID         `json:"applicationId"`
	CredentialID    CredentialID          `json:"credentialId"`
	DelegatedUserID *UserID               `json:"delegatedUserId,omitempty"`
	Environment     Environment           `json:"environment"`
	Outcome         RequestOutcome        `json:"outcome"`
	UsageSource     UsageSource           `json:"usageSource"`
	Units           []UsageQuantity       `json:"units"`
	Model           ModelReference        `json:"model"`
	Labels          RequestLabels         `json:"labels,omitempty"`
	SettledAt       Timestamp             `json:"settledAt"`
	MeteredUsageID  string                `json:"meteredUsageId"`
	ParentRequestID *RequestID            `json:"parentRequestId,omitempty"`
	Tariff          MeteredTariff         `json:"tariff"`
	CustomerCharge  MeteredCustomerCharge `json:"customerCharge"`
}
type MeteredTariffStatus string

var meteredTariffStatusValues = []MeteredTariffStatus{"quoted", "unpriced"}

type MeteredTariff struct {
	Status         MeteredTariffStatus `json:"status"`
	Amount         *ExactDecimal       `json:"amount,omitempty"`
	Currency       *CurrencyCode       `json:"currency,omitempty"`
	PriceVersionID *string             `json:"priceVersionId"`
}
type MeteredCustomerCharge struct {
	Status string `json:"status"`
}
