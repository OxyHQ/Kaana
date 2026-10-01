package credentialstore

import (
	"context"
	"errors"
	"github.com/OxyHQ/Kaana/internal/contract"
	"time"
)

// ScopedAttemptClaim is an already-authorized operation's exact execution binding.
// It grants no authority; the caller must validate signed audience and eligibility.
// PermitID is stable across all HTTP request IDs for this single-use permit.
// It must come from verified private approval, never a caller-supplied request ID.
type ScopedAttemptClaim struct {
	PermitID     string
	DeploymentID contract.DeploymentID
	Scope
	ClaimedAt time.Time
	ExpiresAt time.Time
}

var ErrScopedAttemptUnavailable = errors.New("scoped attempt claim unavailable")

// ClaimScopedAttempt returns true only for a new durable claim. A false result or
// any error must prevent sending; a claim is never released after uncertain work.
func (p *Postgres) ClaimScopedAttempt(ctx context.Context, claim ScopedAttemptClaim) (bool, error) {
	var claimed bool
	err := p.pool.QueryRow(ctx, `SELECT kaana_record_provider_credential_attempt($1,$2,0,$3,$4,'attempt_claimed','local_admission',$5,$6)`, claim.PermitID, claim.DeploymentID, claim.Provider, claim.KeyID, claim.ClaimedAt, claim.ExpiresAt).Scan(&claimed)
	if err != nil {
		return false, ErrScopedAttemptUnavailable
	}
	return claimed, nil
}
