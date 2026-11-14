package peablebilling

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/0003_payments.sql
var paymentMigration string

// MigratePayments is an explicit offline operation. It grants only the already
// provisioned reconciler role; construction and serving do not invoke it.
func (s *Store) MigratePayments(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var role string
	if err = tx.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "kaana_billing_migrator" {
		return ErrMismatch
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-peable-drafts'))`); err != nil {
		return err
	}
	for _, dependency := range []struct{ table, version, source string }{{"kaana_peable_draft_migrations", "0001", migration}, {"kaana_peable_close_migrations", "0002", closeMigration}} {
		var checksum string
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*),coalesce(max(checksum) FILTER(WHERE version=$1),'') FROM public.`+pgx.Identifier{dependency.table}.Sanitize(), dependency.version).Scan(&count, &checksum); err != nil {
			return ErrMismatch
		}
		sum := sha256.Sum256([]byte(dependency.source))
		if count != 1 || checksum != hex.EncodeToString(sum[:]) {
			return ErrMismatch
		}
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.kaana_peable_payment_migrations(version text PRIMARY KEY,checksum text NOT NULL);REVOKE ALL ON public.kaana_peable_payment_migrations FROM PUBLIC`); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(paymentMigration))
	checksum := hex.EncodeToString(sum[:])
	var unknown bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.kaana_peable_payment_migrations WHERE version<>'0003')`).Scan(&unknown); err != nil {
		return err
	}
	if unknown {
		return ErrMismatch
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_peable_payment_migrations WHERE version='0003'`).Scan(&previous)
	if err == nil {
		if previous != checksum {
			return ErrMismatch
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, paymentMigration); err != nil {
		return fmt.Errorf("apply payment migration: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_peable_payment_migrations VALUES('0003',$1)`, checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type PaymentReceipt struct {
	ID        string          `json:"id"`
	DraftID   string          `json:"draftId"`
	Principal string          `json:"principal"`
	Evidence  PaymentEvidence `json:"evidence"`
}

func (s *Store) RecordPayment(ctx context.Context, draft string, evidence PaymentEvidence) (PaymentReceipt, error) {
	var result PaymentReceipt
	raw, err := json.Marshal(evidence)
	if err != nil {
		return result, err
	}
	err = s.closeQuery(ctx, &result, `SELECT kaana_peable_billing.record_payment($1,$2::jsonb)`, draft, raw)
	return result, err
}
