package peablebilling

import (
	"context"
	"time"
)

// DraftStore is the durable state, including an attempt marker committed BEFORE
// an external create. A lost HTTP/DB response never resets that marker.
type DraftStore interface {
	Read(context.Context, string) (Draft, error)
	Claim(context.Context, string, string) (bool, error)
	Record(context.Context, string, string, string) error
}
type Worker struct {
	store  DraftStore
	client *Client
}

func NewWorker(store DraftStore, client *Client) (*Worker, error) {
	if store == nil || client == nil {
		return nil, ErrDisabled
	}
	return &Worker{store: store, client: client}, nil
}

// Deliver creates or reconciles one draft and one fixed-amount item, never
// finalizes, sends or pays. No constructor starts a timer or enrolls an account.
func (w *Worker) Deliver(ctx context.Context, id string) (Draft, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	d, err := w.store.Read(ctx, id)
	if err != nil {
		return d, err
	}
	if err = w.client.Verify(ctx, d); err != nil {
		return d, err
	}
	if d.InvoiceID == "" {
		fresh, e := w.store.Claim(ctx, id, "invoice")
		if e != nil {
			return d, e
		}
		var external string
		if fresh {
			external, err = w.client.CreateInvoice(ctx, d)
		} else {
			external, err = w.client.Find(ctx, d, "invoice")
		}
		if err != nil {
			return d, err
		}
		if err = w.store.Record(ctx, id, "invoice", external); err != nil {
			return d, err
		}
		d, err = w.store.Read(ctx, id)
		if err != nil {
			return d, err
		}
	}
	if err = w.client.ReadInvoice(ctx, d); err != nil {
		return d, err
	}
	if d.ItemID == "" {
		fresh, e := w.store.Claim(ctx, id, "item")
		if e != nil {
			return d, e
		}
		var external string
		if fresh {
			external, err = w.client.CreateItem(ctx, d)
		} else {
			external, err = w.client.Find(ctx, d, "item")
		}
		if err != nil {
			return d, err
		}
		if err = w.store.Record(ctx, id, "item", external); err != nil {
			return d, err
		}
		d, err = w.store.Read(ctx, id)
		if err != nil {
			return d, err
		}
	}
	if err = w.client.ReadItem(ctx, d); err != nil {
		return d, err
	}
	if err = w.client.Complete(ctx, d); err != nil {
		return d, err
	}
	return d, nil
}

// ReconcileEvent treats a webhook/event ID only as a wakeup. The object and its
// ownership are retrieved through Kaana's own authenticated Peable connection;
// no webhook body, claimed payment amount or payer identity is trusted. There
// is intentionally no public handler and no payment/funding action in this slice.
func (w *Worker) ReconcileEvent(ctx context.Context, eventID string) (Draft, error) {
	ref, err := w.client.DraftForEvent(ctx, eventID)
	if err != nil {
		return Draft{}, err
	}
	d, err := w.store.Read(ctx, ref.DraftID)
	if err != nil {
		return d, err
	}
	if d.AccountID != ref.AccountID {
		return d, ErrMismatch
	}
	return w.Deliver(ctx, ref.DraftID)
}
