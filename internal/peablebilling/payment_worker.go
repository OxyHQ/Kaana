package peablebilling

import (
	"context"
	"time"
)

type PaymentStore interface {
	Read(context.Context, string) (Draft, error)
	ReadClose(context.Context, string, string) (CloseAttempt, error)
	RecordPayment(context.Context, string, PaymentEvidence) (PaymentReceipt, error)
}
type PaymentWorker struct {
	store  PaymentStore
	client *Client
}

// An explicit opt-in and a separately granted reconciliation role are required.
// This worker never sends a payment, capture, invoice or refund request.
func NewPaymentWorker(enabled bool, store PaymentStore, client *Client) (*PaymentWorker, error) {
	if !enabled || store == nil || client == nil {
		return nil, ErrDisabled
	}
	return &PaymentWorker{store: store, client: client}, nil
}
func (w *PaymentWorker) Reconcile(ctx context.Context, id, closeKey string) (PaymentReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !operatorID(id) || !operatorID(closeKey) {
		return PaymentReceipt{}, ErrMismatch
	}
	d, err := w.store.Read(ctx, id)
	if err != nil {
		return PaymentReceipt{}, err
	}
	closed, err := w.store.ReadClose(ctx, id, closeKey)
	if err != nil {
		return PaymentReceipt{}, err
	}
	proof, err := w.client.PaidEvidence(ctx, d, closed)
	if err != nil {
		return PaymentReceipt{}, err
	}
	return w.store.RecordPayment(ctx, id, proof)
}
