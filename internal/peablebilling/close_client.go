package peablebilling

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
)

var ErrFinalizationConflict = errors.New("peable billing: reviewed finalization conflict requires reconciliation")
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var unsignedMinorPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,31})$`)
var signedMinorPattern = regexp.MustCompile(`^(0|-?[1-9][0-9]{0,31})$`)

type reviewedInvoice struct {
	Invoice
	Revision             *string           `json:"finalization_revision"`
	AcceptedRevision     *string           `json:"accepted_finalization_revision"`
	Number               *string           `json:"number"`
	Subtotal             string            `json:"subtotal"`
	SubtotalExcludingTax string            `json:"subtotal_excluding_tax"`
	Tax                  *string           `json:"tax"`
	Total                string            `json:"total"`
	TotalExcludingTax    string            `json:"total_excluding_tax"`
	Discounts            []json.RawMessage `json:"total_discount_amounts"`
	StartingBalance      string            `json:"starting_balance"`
	EndingBalance        *string           `json:"ending_balance"`
	AmountDue            string            `json:"amount_due"`
	AmountPaid           string            `json:"amount_paid"`
}

func (c *Client) reviewedInvoice(ctx context.Context, d Draft) (reviewedInvoice, error) {
	var v reviewedInvoice
	err := c.request(ctx, http.MethodGet, "/v1/invoices/"+url.PathEscape(d.InvoiceID)+"?include_finalization_revision=true", "", nil, &v)
	if err != nil {
		return v, err
	}
	// Common identity/terms/line checks remain mandatory after issue.
	identity := v.Invoice
	identity.Status = "draft"
	if !c.validInvoice(identity, d) || v.ID != d.InvoiceID || v.Lines.HasMore == nil || *v.Lines.HasMore || len(v.Lines.Data) != 1 || v.Discounts == nil || len(v.Discounts) != 0 {
		return v, ErrMismatch
	}
	line := v.Lines.Data[0]
	if line.Item != d.ItemID || line.Amount != d.AmountMinor || line.Currency != d.Currency {
		return v, ErrMismatch
	}
	return v, nil
}
func (c *Client) ReviewClose(ctx context.Context, d Draft) (string, error) {
	v, err := c.reviewedInvoice(ctx, d)
	if err != nil {
		return "", err
	}
	if v.Status != "draft" || v.AcceptedRevision != nil || v.Revision == nil || !revisionPattern.MatchString(*v.Revision) {
		return "", ErrMismatch
	}
	return *v.Revision, nil
}
func (c *Client) Finalize(ctx context.Context, d Draft, attempt CloseAttempt) error {
	if !c.cfg.FinalizationEnabled {
		return ErrDisabled
	}
	if !revisionPattern.MatchString(attempt.Revision) || attempt.DraftID != d.ID || attempt.ID == "" || attempt.State != "attempted" {
		return ErrMismatch
	}
	var response json.RawMessage
	return c.request(ctx, http.MethodPost, "/v1/invoices/"+url.PathEscape(d.InvoiceID)+"/finalize", "kaana-close:"+attempt.ID, map[string]string{"expected_finalization_revision": attempt.Revision}, &response)
}
func (c *Client) ClosedEvidence(ctx context.Context, d Draft, revision string) (CloseEvidence, error) {
	v, err := c.reviewedInvoice(ctx, d)
	if err != nil {
		return CloseEvidence{}, err
	}
	if (v.Status != "open" && v.Status != "paid") || v.AcceptedRevision == nil || *v.AcceptedRevision != revision || !revisionPattern.MatchString(revision) || v.Revision != nil || v.Number == nil || !operatorID(*v.Number) || v.Tax == nil || v.EndingBalance == nil || v.AutomaticTax.Status == nil || *v.AutomaticTax.Status != "complete" {
		return CloseEvidence{}, ErrReconcile
	}
	if v.Subtotal != d.AmountMinor || v.TotalExcludingTax != v.SubtotalExcludingTax || !unsignedMinorPattern.MatchString(v.SubtotalExcludingTax) || !unsignedMinorPattern.MatchString(*v.Tax) || !unsignedMinorPattern.MatchString(v.Total) || !unsignedMinorPattern.MatchString(v.AmountDue) || !unsignedMinorPattern.MatchString(v.AmountPaid) || !signedMinorPattern.MatchString(v.StartingBalance) || !signedMinorPattern.MatchString(*v.EndingBalance) {
		return CloseEvidence{}, ErrMismatch
	}
	base, ok := new(big.Int).SetString(d.AmountMinor, 10)
	if !ok {
		return CloseEvidence{}, ErrMismatch
	}
	netBase, _ := new(big.Int).SetString(v.SubtotalExcludingTax, 10)
	tax, _ := new(big.Int).SetString(*v.Tax, 10)
	total, _ := new(big.Int).SetString(v.Total, 10)
	if netBase.Cmp(base) > 0 || total.Cmp(base) < 0 || new(big.Int).Add(netBase, tax).String() != v.Total {
		return CloseEvidence{}, ErrMismatch
	}
	e := CloseEvidence{NetBaseMinor: v.TotalExcludingTax, InvoiceID: d.InvoiceID, Revision: revision, Status: v.Status, Number: *v.Number, BaseMinor: d.AmountMinor, TaxMinor: *v.Tax, TotalMinor: v.Total, StartingBalanceMinor: v.StartingBalance, EndingBalanceMinor: *v.EndingBalance, AmountDueMinor: v.AmountDue, AmountPaidMinor: v.AmountPaid}
	raw, err := json.Marshal(e)
	if err != nil {
		return CloseEvidence{}, err
	}
	sum := sha256.Sum256(raw)
	e.Digest = hex.EncodeToString(sum[:])
	return e, nil
}
