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
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrCloseMissing = errors.New("peable billing: close attempt absent")

//go:embed migrations/0002_close.sql
var closeMigration string

// MigrateClose is a separate explicit migration: existing draft migrations and
// deployments never install financial close authority. Its owner must already
// own billing and drafts; the separate NOLOGIN closer group is provisioned first.
func (s *Store) MigrateClose(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin invoice close migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var role string
	if err = tx.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil || role != "kaana_billing_migrator" {
		return ErrMismatch
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-peable-drafts'))`); err != nil {
		return err
	}
	var draftChecksum string
	if err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_peable_draft_migrations WHERE version='0001'`).Scan(&draftChecksum); err != nil {
		return ErrMismatch
	}
	draftHash := sha256.Sum256([]byte(migration))
	if draftChecksum != hex.EncodeToString(draftHash[:]) {
		return ErrMismatch
	}
	var unexpectedDraft bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.kaana_peable_draft_migrations WHERE version<>'0001')`).Scan(&unexpectedDraft); err != nil {
		return err
	}
	if unexpectedDraft {
		return ErrMismatch
	}

	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.kaana_peable_close_migrations(version text PRIMARY KEY,checksum text NOT NULL);REVOKE ALL ON public.kaana_peable_close_migrations FROM PUBLIC`); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(closeMigration))
	checksum := hex.EncodeToString(sum[:])
	var unknown bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.kaana_peable_close_migrations WHERE version<>'0002')`).Scan(&unknown); err != nil {
		return err
	}
	if unknown {
		return ErrMismatch
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_peable_close_migrations WHERE version='0002'`).Scan(&previous)
	if err == nil {
		if previous != checksum {
			return ErrMismatch
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, closeMigration); err != nil {
		return fmt.Errorf("apply invoice close migration: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_peable_close_migrations VALUES('0002',$1)`, checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CloseEvidence freezes authenticated financial references and exact minor
// amounts at observation. Remote status and amountPaid can change later; this is
// an issuance snapshot, not a live payment projection. It requires the SAME revision;
// a matching amount alone cannot authorize recovery of an ambiguous finalization.
type CloseEvidence struct {
	NetBaseMinor         string `json:"netBaseMinor"`
	StartingBalanceMinor string `json:"startingBalanceMinor"`
	EndingBalanceMinor   string `json:"endingBalanceMinor"`
	AmountDueMinor       string `json:"amountDueMinor"`
	AmountPaidMinor      string `json:"amountPaidMinor"`
	InvoiceID            string `json:"invoiceId"`
	Revision             string `json:"revision"`
	Status               string `json:"status"`
	Number               string `json:"number"`
	Digest               string `json:"digest"`
	BaseMinor            string `json:"baseMinor"`
	TaxMinor             string `json:"taxMinor"`
	TotalMinor           string `json:"totalMinor"`
}
type CloseAttempt struct {
	ID       string         `json:"id"`
	DraftID  string         `json:"draftId"`
	Key      string         `json:"key"`
	Revision string         `json:"revision"`
	State    string         `json:"state"`
	Evidence *CloseEvidence `json:"evidence"`
}
type CloseClaim struct {
	Fresh   bool         `json:"fresh"`
	Attempt CloseAttempt `json:"attempt"`
}

func (s *Store) ClaimClose(ctx context.Context, draft, key, revision string) (CloseClaim, error) {
	var out CloseClaim
	err := s.closeQuery(ctx, &out, `SELECT kaana_peable_billing.claim_close($1,$2,$3)`, draft, key, revision)
	return out, err
}
func (s *Store) ReadClose(ctx context.Context, draft, key string) (CloseAttempt, error) {
	var out CloseAttempt
	err := s.closeQuery(ctx, &out, `SELECT kaana_peable_billing.read_close($1,$2)`, draft, key)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "KP001" {
		return out, ErrCloseMissing
	}
	return out, err
}
func (s *Store) RecordClose(ctx context.Context, draft, key string, evidence CloseEvidence) (CloseAttempt, error) {
	var out CloseAttempt
	raw, err := json.Marshal(evidence)
	if err != nil {
		return out, err
	}
	err = s.closeQuery(ctx, &out, `SELECT kaana_peable_billing.record_close($1,$2,$3::jsonb)`, draft, key, raw)
	return out, err
}
func (s *Store) closeQuery(ctx context.Context, out any, query string, args ...any) error {
	var raw []byte
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		return fmt.Errorf("invoice close state: %w", err)
	}
	return json.Unmarshal(raw, out)
}
