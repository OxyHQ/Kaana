package credentialstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/sessionowner"
	"github.com/jackc/pgx/v5"
)

// RealtimeOwners exposes only the bounded runtime placement functions. It does
// not grant direct table access to a serving database role.
type RealtimeOwners struct{ Store *Postgres }

func (d RealtimeOwners) Claim(ctx context.Context, entry sessionowner.Entry) (bool, error) {
	var ok bool
	err := d.Store.pool.QueryRow(ctx, "SELECT kaana_claim_realtime_owner($1,$2,$3::inet,$4)", entry.RequestID, entry.Lane, entry.Address, entry.Generation).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("claiming realtime owner: %w", err)
	}
	return ok, nil
}
func (d RealtimeOwners) Lookup(ctx context.Context, requestID, lane string) (sessionowner.Entry, bool, error) {
	var e sessionowner.Entry
	err := d.Store.pool.QueryRow(ctx, "SELECT request_id,lane,owner_address,owner_generation,expires_at FROM kaana_lookup_realtime_owner($1,$2)", requestID, lane).Scan(&e.RequestID, &e.Lane, &e.Address, &e.Generation, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("reading realtime owner: %w", err)
	}
	return e, true, nil
}
func (d RealtimeOwners) Renew(ctx context.Context, entry sessionowner.Entry) (bool, error) {
	var ok bool
	err := d.Store.pool.QueryRow(ctx, "SELECT kaana_renew_realtime_owner($1,$2,$3::inet,$4)", entry.RequestID, entry.Lane, entry.Address, entry.Generation).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("renewing realtime owner: %w", err)
	}
	return ok, nil
}
func (d RealtimeOwners) Release(ctx context.Context, entry sessionowner.Entry) error {
	_, err := d.Store.pool.Exec(ctx, "SELECT kaana_release_realtime_owner($1,$2,$3::inet,$4)", entry.RequestID, entry.Lane, entry.Address, entry.Generation)
	if err != nil {
		return fmt.Errorf("releasing realtime owner: %w", err)
	}
	return nil
}
