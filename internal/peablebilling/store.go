// Package peablebilling delivers explicitly prepared Kaana receivables as Peable
// drafts. Its separately opted-in close worker can issue a reviewed invoice;
// neither worker creates a merchant, funds an account or marks a payment collected.
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
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0001_drafts.sql
var migration string

var (
	ErrDisabled  = errors.New("peable billing: disabled")
	ErrMismatch  = errors.New("peable billing: authoritative mapping mismatch")
	ErrReconcile = errors.New("peable billing: ambiguous delivery requires reconciliation")
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate is offline-only under the owner of the existing billing schema.
// It creates no roles, mappings, accounts, drafts or funds.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin draft migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner string
	if err = tx.QueryRow(ctx, `SELECT current_user`).Scan(&owner); err != nil {
		return fmt.Errorf("read migration role: %w", err)
	}
	if owner != "kaana_billing_migrator" {
		return ErrMismatch
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-peable-drafts')); SELECT 1 FROM kaana_billing.receipts LIMIT 0; CREATE TABLE IF NOT EXISTS public.kaana_peable_draft_migrations(version text PRIMARY KEY,checksum text NOT NULL); REVOKE ALL ON public.kaana_peable_draft_migrations FROM PUBLIC`); err != nil {
		return fmt.Errorf("prepare draft migration: %w", err)
	}
	sum := sha256.Sum256([]byte(migration))
	checksum := hex.EncodeToString(sum[:])
	var unknown bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.kaana_peable_draft_migrations WHERE version<>'0001')`).Scan(&unknown); err != nil {
		return fmt.Errorf("check draft migration: %w", err)
	}
	if unknown {
		return ErrMismatch
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_peable_draft_migrations WHERE version='0001'`).Scan(&previous)
	if err == nil {
		if previous != checksum {
			return ErrMismatch
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read draft migration: %w", err)
	}
	if _, err = tx.Exec(ctx, migration); err != nil {
		return fmt.Errorf("apply draft migration: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_peable_draft_migrations VALUES('0001',$1)`, checksum); err != nil {
		return fmt.Errorf("record draft migration: %w", err)
	}
	return tx.Commit(ctx)
}

type Draft struct {
	ID                 string `json:"id"`
	AccountID          string `json:"accountId"`
	MerchantID         string `json:"merchantId"`
	CustomerID         string `json:"customerId"`
	Currency           string `json:"currency"`
	AmountMinor        string `json:"amountMinor"`
	Subtotal           string `json:"subtotal"`
	RoundingAdjustment string `json:"roundingAdjustment"`
	MinorExponent      int    `json:"minorExponent"`
	DaysUntilDue       int    `json:"daysUntilDue"`
	InvoiceAttempted   bool   `json:"invoiceAttempted"`
	InvoiceID          string `json:"invoiceId"`
	ItemAttempted      bool   `json:"itemAttempted"`
	ItemID             string `json:"itemId"`
}

func (s *Store) BindCustomer(ctx context.Context, account, merchant, customer string) error {
	_, err := s.pool.Exec(ctx, `SELECT kaana_peable_billing.bind_customer($1,$2,$3)`, account, merchant, customer)
	return err
}
func (s *Store) Prepare(ctx context.Context, account, key string, receipts []string, days int) (Draft, error) {
	return s.query(ctx, `SELECT kaana_peable_billing.prepare_draft($1,$2,$3,$4)`, account, key, receipts, days)
}
func (s *Store) Read(ctx context.Context, id string) (Draft, error) {
	return s.query(ctx, `SELECT kaana_peable_billing.read_draft($1)`, id)
}
func (s *Store) query(ctx context.Context, query string, args ...any) (Draft, error) {
	var raw []byte
	var d Draft
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		return d, fmt.Errorf("read draft: %w", err)
	}
	err := json.Unmarshal(raw, &d)
	return d, err
}
func (s *Store) Claim(ctx context.Context, id, step string) (bool, error) {
	var fresh bool
	err := s.pool.QueryRow(ctx, `SELECT kaana_peable_billing.claim_step($1,$2)`, id, step).Scan(&fresh)
	return fresh, err
}
func (s *Store) Record(ctx context.Context, id, step, external string) error {
	_, err := s.pool.Exec(ctx, `SELECT kaana_peable_billing.record_step($1,$2,$3)`, id, step, external)
	return err
}
