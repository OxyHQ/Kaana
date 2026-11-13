// Package billing is Kaana's disabled commercial-ledger foundation. It accepts
// rated amounts and immutable evidence references, never prompts or price rules.
package billing

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0001_ledger.sql
var migration string

var (
	ErrImmutable     = errors.New("billing: immutable record")
	ErrInvalid       = errors.New("billing: invalid input")
	ErrNotFound      = errors.New("billing: scoped record not found")
	ErrConflict      = errors.New("billing: idempotency conflict")
	ErrDisabled      = errors.New("billing: account disabled")
	ErrBudget        = errors.New("billing: budget exhausted or outside its window")
	ErrFunds         = errors.New("billing: insufficient funds")
	ErrExceedsHold   = errors.New("billing: settlement exceeds hold")
	ErrExceedsCharge = errors.New("billing: refund exceeds charge")
)

// Store must use a dedicated billing role. No constructor migrates, provisions,
// funds or activates an account. Money is always an exact base-10 string.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate runs only with the separately provisioned migration role. It neither
// creates roles nor grants an inference worker migration privileges.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return wrap(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-billing-migrations'))`); err != nil {
		return wrap(err)
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.kaana_billing_migrations(version text PRIMARY KEY,checksum text NOT NULL); REVOKE ALL ON public.kaana_billing_migrations FROM PUBLIC`); err != nil {
		return wrap(err)
	}
	sum := sha256.Sum256([]byte(migration))
	checksum := hex.EncodeToString(sum[:])
	var previous string
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_billing_migrations WHERE version='0001'`).Scan(&previous)
	if err == nil {
		if previous != checksum {
			return fmt.Errorf("%w: migration checksum", ErrConflict)
		}
		return wrap(tx.Commit(ctx))
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return wrap(err)
	}
	if _, err = tx.Exec(ctx, migration); err != nil {
		return wrap(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_billing_migrations VALUES('0001',$1)`, checksum); err != nil {
		return wrap(err)
	}
	return wrap(tx.Commit(ctx))
}

// Account IDs and currency IDs are minted by Kaana; OxyOwner is never a key.
type Account struct {
	ID         string `json:"id"`
	CurrencyID string `json:"currencyId"`
	Enabled    bool   `json:"enabled"`
}

func (s *Store) CreateAccount(ctx context.Context, oxyOwner, currencyCode string) (Account, error) {
	return queryJSON[Account](ctx, s.pool, `SELECT kaana_billing.create_account($1,$2)`, oxyOwner, currencyCode)
}

type BudgetRequest struct {
	AccountID, Key, Amount string
	StartsAt, EndsAt       time.Time
}

func (s *Store) ConfigureBudget(ctx context.Context, r BudgetRequest) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT kaana_billing.configure_budget($1,$2,$3,$4,$5)`, r.AccountID, r.Key, r.StartsAt, r.EndsAt, r.Amount).Scan(&id)
	return id, wrap(err)
}

// ConfigureAccount is an operator action. A zero credit limit means prepaid;
// positive credit is an explicit invoiced ceiling, never inferred enrollment.
func (s *Store) ConfigureAccount(ctx context.Context, accountID, budgetID, creditLimit string, enabled bool) error {
	_, err := s.pool.Exec(ctx, `SELECT kaana_billing.configure_account($1,$2,$3,$4)`, accountID, budgetID, creditLimit, enabled)
	return wrap(err)
}

type FundingRequest struct{ AccountID, Key, Kind, Amount, EvidenceID string }

// Fund requires already verified external payment or approved promotional grant
// evidence. This library does not verify a payment, manufacture it or pay out.
func (s *Store) Fund(ctx context.Context, r FundingRequest) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT kaana_billing.fund($1,$2,$3,$4,$5)`, r.AccountID, r.Key, r.Kind, r.Amount, r.EvidenceID).Scan(&id)
	return id, wrap(err)
}

type ReserveRequest struct {
	AccountID, Key, AdmissionID, PriceSnapshotID, PriceDigest, Maximum string
	TTL                                                                time.Duration
}
type Hold struct {
	Replay    bool      `json:"replay"`
	ID        string    `json:"id"`
	AccountID string    `json:"accountId"`
	BudgetID  string    `json:"budgetId"`
	State     string    `json:"state"`
	Amount    string    `json:"amount"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Store) Reserve(ctx context.Context, r ReserveRequest) (Hold, error) {
	return reserveWith(ctx, s.pool, r)
}

// ReserveInTransaction participates in the caller's admission transaction.
func ReserveInTransaction(ctx context.Context, tx pgx.Tx, r ReserveRequest) (Hold, error) {
	return reserveWith(ctx, tx, r)
}
func reserveWith(ctx context.Context, db queryer, r ReserveRequest) (Hold, error) {
	if r.TTL < time.Second || r.TTL > 15*time.Minute || r.TTL%time.Second != 0 {
		return Hold{}, ErrInvalid
	}
	return queryJSON[Hold](ctx, db, `SELECT kaana_billing.reserve($1,$2,$3,$4,$5,$6,$7)`, r.AccountID, r.Key, r.AdmissionID, r.PriceSnapshotID, r.PriceDigest, r.Maximum, int(r.TTL/time.Second))
}

type SettleRequest struct {
	AccountID, HoldID, Key, EvidenceID, EvidenceDigest, Outcome, RatedAmount string
	HasUsage                                                                 bool
}
type Receipt struct {
	ID         string `json:"id"`
	AccountID  string `json:"accountId"`
	HoldID     string `json:"holdId"`
	EvidenceID string `json:"evidenceId"`
	Charged    string `json:"charged"`
	Outcome    string `json:"outcome"`
}

// Settlement.State is settled or expired. Only a non-nil receipt proves a
// settlement. Expired holds stay expired: a late report never becomes a debit.
type Settlement struct {
	State   string   `json:"state"`
	Receipt *Receipt `json:"receipt"`
}

func (s *Store) Settle(ctx context.Context, r SettleRequest) (Settlement, error) {
	return settleWith(ctx, s.pool, r)
}

// SettleInTransaction commits the charge with native usage and its public record.
func SettleInTransaction(ctx context.Context, tx pgx.Tx, r SettleRequest) (Settlement, error) {
	return settleWith(ctx, tx, r)
}
func settleWith(ctx context.Context, db queryer, r SettleRequest) (Settlement, error) {
	return queryJSON[Settlement](ctx, db, `SELECT kaana_billing.settle($1,$2,$3,$4,$5,$6,$7,$8)`, r.AccountID, r.HoldID, r.Key, r.EvidenceID, r.EvidenceDigest, r.Outcome, r.HasUsage, r.RatedAmount)
}

type RefundRequest struct{ AccountID, ReceiptID, Key, Amount, EvidenceID string }

// Refund compensates inference consumption in reverse draw order. It is NOT a
// payment-processor refund: promotional funds are never converted into cash.
func (s *Store) Refund(ctx context.Context, r RefundRequest) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT kaana_billing.refund($1,$2,$3,$4,$5)`, r.AccountID, r.ReceiptID, r.Key, r.Amount, r.EvidenceID).Scan(&id)
	return id, wrap(err)
}

// Expire releases a bounded batch. Each hold is an atomic transaction; a failed
// batch reports the committed count so retrying cannot double-release money.
func (s *Store) Expire(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM kaana_billing.holds WHERE state='held' AND expires_at<=clock_timestamp() ORDER BY expires_at,id LIMIT $1`, limit)
	if err != nil {
		return 0, wrap(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, wrap(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, wrap(err)
	}
	count := 0
	for _, id := range ids {
		var released bool
		err = s.pool.QueryRow(ctx, `SELECT kaana_billing.expire($1)`, id).Scan(&released)
		if err != nil {
			return count, wrap(err)
		}
		if released {
			count++
		}
	}
	return count, nil
}

type Balance struct{ Promotional, Purchased, Invoiced, Reserved string }

func (s *Store) Balance(ctx context.Context, accountID string) (Balance, error) {
	var b Balance
	err := s.pool.QueryRow(ctx, `SELECT promotional::text,purchased::text,invoiced::text,reserved::text FROM kaana_billing.accounts WHERE id=$1`, accountID).Scan(&b.Promotional, &b.Purchased, &b.Invoiced, &b.Reserved)
	return b, wrap(err)
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func queryJSON[T any](ctx context.Context, pool queryer, query string, args ...any) (T, error) {
	var result T
	var raw []byte
	if err := pool.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		return result, wrap(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, fmt.Errorf("billing: decode result: %w", err)
	}
	return result, nil
}
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		kinds := map[string]error{"KB001": ErrImmutable, "KB002": ErrInvalid, "KB003": ErrNotFound, "KB004": ErrConflict, "KB005": ErrDisabled, "KB006": ErrBudget, "KB007": ErrFunds, "KB008": ErrExceedsHold, "KB009": ErrExceedsCharge, "23505": ErrConflict, "23503": ErrInvalid, "23514": ErrInvalid}
		if known, ok := kinds[pg.Code]; ok {
			return known
		}
	}
	return fmt.Errorf("billing: database operation: %w", err)
}

// AccountForOwnerInTransaction resolves a trusted principal's opaque owner and
// the rated currency. It neither provisions an account nor substitutes a user.
func AccountForOwnerInTransaction(ctx context.Context, tx pgx.Tx, owner, currency string) (Account, error) {
	var a Account
	err := tx.QueryRow(ctx, `SELECT a.id,a.currency_id,a.enabled FROM kaana_billing.accounts a JOIN kaana_billing.currencies c ON c.id=a.currency_id WHERE a.oxy_owner=$1 AND c.code=$2`, owner, currency).Scan(&a.ID, &a.CurrencyID, &a.Enabled)
	return a, wrap(err)
}
