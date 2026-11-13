package nativeadmission

import (
	"context"
	"errors"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5"
)

func sameGeneration(known, incoming *contract.GenerationID) bool {
	return known == nil || (incoming != nil && *known == *incoming)
}

// BindGeneration makes every generation id visible in a stream durably owned
// by its admission first, including incomplete and foreign-owned lookups.
func (s *Store) BindGeneration(ctx context.Context, admissionID string, id contract.GenerationID) error {
	if id == "" {
		return ErrInvalidRequest
	}
	result, err := s.pool.Exec(ctx, `UPDATE kaana_admission.admissions SET generation_id=$2 WHERE id=$1 AND (generation_id IS NULL OR generation_id=$2)`, admissionID, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// HasNativeGeneration answers authority ownership, not permission to read it.
// A foreign or pending native record must not trigger a historical Oxy lookup.
func (s *Store) HasNativeGeneration(ctx context.Context, credential contract.CredentialID, id *contract.GenerationID, key *contract.IdempotencyKey) (bool, error) {
	if (id == nil) == (key == nil) {
		return false, ErrInvalidRequest
	}
	var found bool
	var err error
	if id != nil {
		err = s.pool.QueryRow(ctx, `SELECT true FROM kaana_admission.admissions WHERE generation_id=$1 OR request_id=$1`, *id).Scan(&found)
	} else {
		scope, value := idempotencyIdentity(credential, key, "")
		err = s.pool.QueryRow(ctx, `SELECT true FROM kaana_admission.admissions WHERE idempotency_scope=$1 AND idempotency_key=$2`, scope, value).Scan(&found)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return found, err
}
