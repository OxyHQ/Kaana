package peablebilling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
)

// PaymentEvidence records a merchant-authenticated observation, not an assertion
// that a card payment cannot later be refunded or disputed. Those later events
// require their own evidence and compensating reconciliation.
type PaymentEvidence struct {
	InvoiceID       string `json:"invoiceId"`
	Revision        string `json:"revision"`
	MerchantID      string `json:"merchantId"`
	CustomerID      string `json:"customerId"`
	Currency        string `json:"currency"`
	PaymentIntentID string `json:"paymentIntentId"`
	ChargeID        string `json:"chargeId"`
	TotalMinor      string `json:"totalMinor"`
	BaseMinor       string `json:"baseMinor"`
	Digest          string `json:"digest"`
}

type paidInvoice struct {
	reviewedInvoice
	Remaining  string `json:"amount_remaining"`
	PreCredit  string `json:"pre_payment_credit_notes_amount"`
	PostCredit string `json:"post_payment_credit_notes_amount"`
	IntentID   string `json:"payment_intent"`
	OutOfBand  *bool  `json:"paid_out_of_band"`
}

type paidIntent struct {
	ID         string          `json:"id"`
	Object     string          `json:"object"`
	Live       *bool           `json:"livemode"`
	MerchantID string          `json:"merchant_id"`
	CustomerID string          `json:"customer"`
	Currency   string          `json:"currency"`
	Status     string          `json:"status"`
	Rail       string          `json:"rail"`
	Amount     string          `json:"amount"`
	Received   string          `json:"amount_received"`
	Refunded   string          `json:"amount_refunded"`
	Capturable string          `json:"amount_capturable"`
	ChargeID   string          `json:"latest_charge"`
	Connected  json.RawMessage `json:"connected_account"`
	OnBehalfOf json.RawMessage `json:"on_behalf_of"`
	Transfer   json.RawMessage `json:"transfer_data"`
}

type paidCharge struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Live       *bool  `json:"livemode"`
	IntentID   string `json:"payment_intent"`
	CustomerID string `json:"customer"`
	Currency   string `json:"currency"`
	Status     string `json:"status"`
	Amount     string `json:"amount"`
	Captured   string `json:"amount_captured"`
	Refunded   string `json:"amount_refunded"`
	Paid       *bool  `json:"paid"`
	IsCaptured *bool  `json:"captured"`
	IsRefunded *bool  `json:"refunded"`
	Disputed   *bool  `json:"disputed"`
}

func boolIs(v *bool, expected bool) bool { return v != nil && *v == expected }

// PaidEvidence uses existing Peable GET resources only. The first settlement
// slice accepts a fully paid direct card invoice without balance application,
// credit notes or manual payment. Other rails/adjustments remain reconciliations,
// not zero amounts or permission to infer an allocation of tax and principal.
func (c *Client) PaidEvidence(ctx context.Context, d Draft, closed CloseAttempt) (PaymentEvidence, error) {
	if closed.DraftID != d.ID || closed.State != "closed" || closed.Evidence == nil || closed.Evidence.Revision != closed.Revision {
		return PaymentEvidence{}, ErrMismatch
	}
	if err := c.Verify(ctx, d); err != nil {
		return PaymentEvidence{}, err
	}
	issuance, err := c.ClosedEvidence(ctx, d, closed.Revision)
	if err != nil {
		return PaymentEvidence{}, err
	}
	prior := closed.Evidence
	if issuance.Number != prior.Number || issuance.BaseMinor != prior.BaseMinor || issuance.NetBaseMinor != prior.NetBaseMinor || issuance.TaxMinor != prior.TaxMinor || issuance.TotalMinor != prior.TotalMinor {
		return PaymentEvidence{}, ErrMismatch
	}
	var invoice paidInvoice
	if err = c.request(ctx, http.MethodGet, "/v1/invoices/"+url.PathEscape(d.InvoiceID)+"?include_finalization_revision=true", "", nil, &invoice); err != nil {
		return PaymentEvidence{}, err
	}
	// Repeat identity and accepted-revision checks on this actual payment read;
	// a preceding issuance read cannot authorize a different returned resource.
	identity := invoice.Invoice
	identity.Status = "draft"
	if !c.validInvoice(identity, d) || invoice.ID != d.InvoiceID || invoice.Status != "paid" || invoice.AcceptedRevision == nil || *invoice.AcceptedRevision != closed.Revision || !boolIs(invoice.OutOfBand, false) || invoice.Remaining != "0" || invoice.PreCredit != "0" || invoice.PostCredit != "0" || invoice.StartingBalance != "0" || invoice.EndingBalance == nil || *invoice.EndingBalance != "0" || invoice.Total != prior.TotalMinor || invoice.AmountDue != invoice.Total || invoice.AmountPaid != invoice.Total || !unsignedMinorPattern.MatchString(invoice.Total) || invoice.Total == "0" || !operatorID(invoice.IntentID) {
		return PaymentEvidence{}, ErrReconcile
	}
	if invoice.Lines.HasMore == nil || *invoice.Lines.HasMore || len(invoice.Lines.Data) != 1 || invoice.Discounts == nil || len(invoice.Discounts) != 0 || invoice.Tax == nil || *invoice.Tax != prior.TaxMinor || invoice.Subtotal != prior.BaseMinor || invoice.TotalExcludingTax != prior.NetBaseMinor {
		return PaymentEvidence{}, ErrMismatch
	}
	line := invoice.Lines.Data[0]
	if line.Item != d.ItemID || line.Amount != d.AmountMinor || line.Currency != d.Currency {
		return PaymentEvidence{}, ErrMismatch
	}
	var intent paidIntent
	if err = c.request(ctx, http.MethodGet, "/v1/payment_intents/"+url.PathEscape(invoice.IntentID), "", nil, &intent); err != nil {
		return PaymentEvidence{}, err
	}
	live := c.cfg.Environment == "production"
	if intent.Object != "payment_intent" || intent.ID != invoice.IntentID || !boolIs(intent.Live, live) || intent.MerchantID != d.MerchantID || intent.CustomerID != d.CustomerID || intent.Currency != d.Currency || intent.Status != "succeeded" || intent.Rail != "card" || intent.Amount != invoice.Total || intent.Received != invoice.Total || intent.Refunded != "0" || intent.Capturable != "0" || !operatorID(intent.ChargeID) || !bytes.Equal(intent.Connected, []byte("null")) || !bytes.Equal(intent.OnBehalfOf, []byte("null")) || !bytes.Equal(intent.Transfer, []byte("null")) {
		return PaymentEvidence{}, ErrReconcile
	}
	var charge paidCharge
	if err = c.request(ctx, http.MethodGet, "/v1/charges/"+url.PathEscape(intent.ChargeID), "", nil, &charge); err != nil {
		return PaymentEvidence{}, err
	}
	if charge.Object != "charge" || charge.ID != intent.ChargeID || !boolIs(charge.Live, live) || charge.IntentID != intent.ID || charge.CustomerID != d.CustomerID || charge.Currency != d.Currency || charge.Status != "succeeded" || charge.Amount != invoice.Total || charge.Captured != invoice.Total || charge.Refunded != "0" || !boolIs(charge.Paid, true) || !boolIs(charge.IsCaptured, true) || !boolIs(charge.IsRefunded, false) || !boolIs(charge.Disputed, false) {
		return PaymentEvidence{}, ErrReconcile
	}
	proof := PaymentEvidence{InvoiceID: d.InvoiceID, Revision: closed.Revision, MerchantID: d.MerchantID, CustomerID: d.CustomerID, Currency: d.Currency, PaymentIntentID: intent.ID, ChargeID: charge.ID, TotalMinor: invoice.Total, BaseMinor: d.AmountMinor}
	raw, err := json.Marshal(proof)
	if err != nil {
		return PaymentEvidence{}, err
	}
	digest := sha256.Sum256(raw)
	proof.Digest = hex.EncodeToString(digest[:])
	return proof, nil
}
