// Package billingaccounts provides explicit reviewed account operations, not
// funding, payment collection, inference enrollment or automatic provisioning.
package billingaccounts

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0001_account_operations.sql
var migration string

var ErrReview = errors.New("billing accounts: reviewed operation or authority refused")

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate verifies the existing billing ledger and uses a separate checksum
// ledger. It does not rewrite any shipped billing/Peable/native migration.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT current_user='kaana_billing_migrator' AND nspowner=current_user::regrole FROM pg_namespace WHERE nspname='kaana_billing'`).Scan(&valid); err != nil || !valid {
		return ErrReview
	}
	if err = billing.VerifyMigration(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-billing-account-operations')); CREATE TABLE IF NOT EXISTS public.kaana_billing_account_migrations(version text PRIMARY KEY,checksum text NOT NULL); REVOKE ALL ON public.kaana_billing_account_migrations FROM PUBLIC`); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(migration))
	checksum := hex.EncodeToString(sum[:])
	var previous string
	var unknown bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.kaana_billing_account_migrations WHERE version<>'0001')`).Scan(&unknown); err != nil || unknown {
		return ErrReview
	}
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_billing_account_migrations WHERE version='0001'`).Scan(&previous)
	if err == nil {
		if previous != checksum {
			return ErrReview
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, migration); err != nil {
		return fmt.Errorf("account operation migration: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_billing_account_migrations VALUES('0001',$1)`, checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Apply returns the committed receipt, or an inspection snapshot. A replay's
// receipt is historical; inspect separately to learn current account state.
func (s *Store) Apply(ctx context.Context, c Config, o Operation) (json.RawMessage, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	var raw []byte
	var err error
	switch o.Action {
	case "provision":
		err = s.pool.QueryRow(ctx, `SELECT kaana_billing_accounts.provision($1,$2,$3,$4,$5)`, c.OwnerAccountID, c.Currency, o.Key, o.EvidenceID, c.Digest).Scan(&raw)
	case "inspect":
		err = s.pool.QueryRow(ctx, `SELECT kaana_billing_accounts.inspect($1,$2,$3)`, c.OwnerAccountID, c.Currency, o.AccountID).Scan(&raw)
	case "configure":
		err = s.pool.QueryRow(ctx, `SELECT kaana_billing_accounts.configure($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, c.OwnerAccountID, c.Currency, o.AccountID, o.Key, o.EvidenceID, c.Digest, o.Revision, o.BudgetKey, o.StartsAt, o.EndsAt, o.Amount, o.CreditLimit, *o.Enabled).Scan(&raw)
	default:
		return nil, ErrReview
	}
	if err != nil {
		return nil, fmt.Errorf("account operation: %w", err)
	}
	return json.RawMessage(raw), nil
}
