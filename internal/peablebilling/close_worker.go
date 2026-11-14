package peablebilling

import (
	"context"
	"errors"
	"time"
)

type CloseStore interface {
	Read(context.Context, string) (Draft, error)
	ReadClose(context.Context, string, string) (CloseAttempt, error)
	ClaimClose(context.Context, string, string, string) (CloseClaim, error)
	RecordClose(context.Context, string, string, CloseEvidence) (CloseAttempt, error)
}
type CloseWorker struct {
	store  CloseStore
	client *Client
}

// Separate explicit opt-in: creating a delivery worker never gains authority to issue.
func NewCloseWorker(enabled bool, store CloseStore, client *Client) (*CloseWorker, error) {
	if !enabled || store == nil || client == nil || !client.cfg.FinalizationEnabled {
		return nil, ErrDisabled
	}
	return &CloseWorker{store: store, client: client}, nil
}
func (w *CloseWorker) Review(ctx context.Context, id string) (string, error) {
	d, err := w.store.Read(ctx, id)
	if err != nil {
		return "", err
	}
	if err = w.client.Verify(ctx, d); err != nil {
		return "", err
	}
	return w.client.ReviewClose(ctx, d)
}

// Close makes at most one POST, after the permanent local claim. A refused or
// ambiguous result never releases receipt net or authorizes another operation
// key. Recovery requires the exact accepted revision, not amount equality.
func (w *CloseWorker) Close(ctx context.Context, id, key, revision string) (CloseAttempt, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !operatorID(key) || !revisionPattern.MatchString(revision) {
		return CloseAttempt{}, ErrMismatch
	}
	d, err := w.store.Read(ctx, id)
	if err != nil {
		return CloseAttempt{}, err
	}
	if err = w.client.Verify(ctx, d); err != nil {
		return CloseAttempt{}, err
	}
	attempt, err := w.store.ReadClose(ctx, id, key)
	if err == nil {
		if attempt.Revision != revision {
			return attempt, ErrMismatch
		}
		if attempt.State == "closed" {
			return attempt, nil
		}
		return w.reconcile(ctx, d, attempt)
	}
	if !errors.Is(err, ErrCloseMissing) {
		return CloseAttempt{}, err
	}
	reviewed, err := w.client.ReviewClose(ctx, d)
	if err != nil {
		return CloseAttempt{}, err
	}
	if reviewed != revision {
		return CloseAttempt{}, ErrMismatch
	}
	claim, err := w.store.ClaimClose(ctx, id, key, revision)
	if err != nil {
		return CloseAttempt{}, err
	}
	if claim.Fresh {
		err = w.client.Finalize(ctx, d, claim.Attempt)
		if err != nil && !errors.Is(err, ErrFinalizationConflict) {
			return claim.Attempt, err
		}
		// A 409 can mean another actor already issued it. Keep the fence and inspect
		// accepted proof; a 409 alone is never permission to refund or repeat a POST.
	}
	return w.reconcile(ctx, d, claim.Attempt)
}
func (w *CloseWorker) reconcile(ctx context.Context, d Draft, attempt CloseAttempt) (CloseAttempt, error) {
	proof, err := w.client.ClosedEvidence(ctx, d, attempt.Revision)
	if err != nil {
		return attempt, err
	}
	return w.store.RecordClose(ctx, d.ID, attempt.Key, proof)
}
