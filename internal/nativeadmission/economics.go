package nativeadmission

import (
	"context"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EconomicTreatment is explicit reviewed policy, never inferred from a brand,
// caller-supplied attribution or the mere existence of a billing account.
type EconomicTreatment string

const (
	InternalMetered EconomicTreatment = "internal_metered"
	Commercial      EconomicTreatment = "commercial"
)

func (t EconomicTreatment) valid() bool { return t == InternalMetered || t == Commercial }

// RatedCompletion is derived from the same immutable plan and evidence as the
// native settlement. RatingFailure must never be turned into a zero receipt.
type RatedCompletion struct {
	Admission      Admission
	Report         contract.UsageReport
	EvidenceDigest string
	Metered        contract.MeteredGenerationRecord
	RatingFailure  *string
}

// EconomicSettlement is either a validated commercial receipt or an explicit
// unavailable outcome. The coordinator persists the latter's reason in its own
// durable outcome record; native reads do not fabricate a free generation.
type EconomicSettlement struct {
	Charge            *contract.ChargedGenerationRecord
	UnavailableReason string
}

// AdmissionEconomics is fixed when constructing the store. Both hooks execute
// inside the native transaction; completion replay cannot bypass settlement.
// The connection needs combined runtime grants, never admin/migration roles.
type AdmissionEconomics interface {
	Reserve(context.Context, pgx.Tx, Admission) error
	Settle(context.Context, pgx.Tx, RatedCompletion) (EconomicSettlement, error)
}

func NewStoreWithEconomics(pool *pgxpool.Pool, economics AdmissionEconomics) *Store {
	return &Store{pool: pool, economics: economics}
}

func (s *Store) requireEconomics(t EconomicTreatment) error {
	if !t.valid() {
		return fmt.Errorf("%w: unknown economic treatment", ErrNotEnrolled)
	}
	if t == Commercial && s.economics == nil {
		return fmt.Errorf("%w: commercial settlement unavailable", ErrDisabled)
	}
	return nil
}
