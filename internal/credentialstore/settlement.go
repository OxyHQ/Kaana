package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OxyHQ/Kaana/internal/providercost"
)

// The settlement outbox (migration 0022; Kaana ADR 0002, decision 3).
//
// A granted request's settlement is written in the same transaction as its
// provider cost events, so Kaana can never hold a cost it did not settle or
// settle a request whose cost it lost. Delivery is a separate worker
// (internal/publicapi) that claims due rows, signs and posts them to Oxy, and
// records Oxy's acknowledgement; every task runs one, and SKIP LOCKED plus a
// lease keeps a row with one deliverer at a time.

// OutboxSettlement is one settlement as the outbox stores it: its identities
// and the exact JSON bytes every delivery sends.
type OutboxSettlement struct {
	SettlementID string
	GrantID      string
	RequestID    string
	Body         []byte
}

// ClaimedSettlement is a due settlement leased to this deliverer.
type ClaimedSettlement struct {
	SettlementID string
	Body         []byte
	// Attempts counts deliveries, this one included.
	Attempts int
}

// PositionedSettlement is one row of the reconciliation feed.
type PositionedSettlement struct {
	Position int64
	Body     []byte
}

// ErrSettlementConflict is returned when a settlement claims an identity the
// outbox already holds with other bytes: one grant settles once.
var ErrSettlementConflict = errors.New("credential store: settlement identity conflict")

var settlementFailurePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// RecordSettlement writes a request's provider cost events and its settlement
// in ONE transaction. Either both are durable or neither is; a retried call
// with the same bytes is a replay, not a second row.
func (p *Postgres) RecordSettlement(ctx context.Context, events []providercost.Event, settlement OutboxSettlement) error {
	if settlement.SettlementID == "" || settlement.GrantID == "" || settlement.RequestID == "" || len(settlement.Body) < 2 {
		return errors.New("credential store: a settlement names itself, its grant and its request")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("credential store: beginning settlement transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(events) > 0 {
		payload, err := encodeProviderCostEvents(events)
		if err != nil {
			return err
		}
		var recorded int
		if err := tx.QueryRow(ctx, `SELECT kaana_record_provider_attempt_events($1::jsonb)`, payload).Scan(&recorded); err != nil {
			return fmt.Errorf("credential store: recording the settlement's provider cost events: %w", err)
		}
		if recorded != len(events) {
			return fmt.Errorf("credential store: provider cost event batch recorded %d of %d attempts", recorded, len(events))
		}
	}
	var outcome string
	if err := tx.QueryRow(ctx, `SELECT kaana_enqueue_settlement($1, $2, $3, $4)`,
		settlement.SettlementID, settlement.GrantID, settlement.RequestID, string(settlement.Body)).Scan(&outcome); err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Message == "settlement identity conflict" {
			return ErrSettlementConflict
		}
		return fmt.Errorf("credential store: enqueuing settlement: %w", err)
	}
	if outcome != "enqueued" && outcome != "replayed" {
		return fmt.Errorf("credential store: enqueuing settlement answered %q", outcome)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("credential store: committing settlement: %w", err)
	}
	return nil
}

// ClaimSettlements leases up to limit due settlements for lease.
func (p *Postgres) ClaimSettlements(ctx context.Context, limit int, lease time.Duration) ([]ClaimedSettlement, error) {
	rows, err := p.pool.Query(ctx, `SELECT settlement_id, body, attempts FROM kaana_claim_settlements($1, $2::interval)`,
		limit, fmt.Sprintf("%d milliseconds", lease.Milliseconds()))
	if err != nil {
		return nil, fmt.Errorf("credential store: claiming settlements: %w", err)
	}
	defer rows.Close()
	claimed := make([]ClaimedSettlement, 0, limit)
	for rows.Next() {
		var row ClaimedSettlement
		var body string
		if err := rows.Scan(&row.SettlementID, &body, &row.Attempts); err != nil {
			return nil, fmt.Errorf("credential store: reading a claimed settlement: %w", err)
		}
		row.Body = []byte(body)
		claimed = append(claimed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: claiming settlements: %w", err)
	}
	return claimed, nil
}

// MarkSettlementDelivered records Oxy's acknowledgement (settled or
// duplicate). Acknowledging a delivered row again changes nothing.
func (p *Postgres) MarkSettlementDelivered(ctx context.Context, settlementID, status string) error {
	var changed bool
	if err := p.pool.QueryRow(ctx, `SELECT kaana_mark_settlement_delivered($1, $2)`, settlementID, status).Scan(&changed); err != nil {
		return fmt.Errorf("credential store: recording a settlement acknowledgement: %w", err)
	}
	return nil
}

// DeferSettlement records a failed delivery as a closed, content-free class
// and when to try again.
func (p *Postgres) DeferSettlement(ctx context.Context, settlementID string, retryAt time.Time, failure string) error {
	if !settlementFailurePattern.MatchString(failure) {
		return fmt.Errorf("credential store: settlement failure class %q is not a closed identifier", failure)
	}
	var changed bool
	if err := p.pool.QueryRow(ctx, `SELECT kaana_defer_settlement($1, $2, $3)`, settlementID, retryAt, failure).Scan(&changed); err != nil {
		return fmt.Errorf("credential store: deferring a settlement: %w", err)
	}
	return nil
}

// ReadSettlements is the reconciliation feed: settlements after a position,
// oldest first, delivered or not.
func (p *Postgres) ReadSettlements(ctx context.Context, afterPosition int64, limit int) ([]PositionedSettlement, error) {
	rows, err := p.pool.Query(ctx, `SELECT outbox_seq, body FROM kaana_read_settlements($1, $2)`, afterPosition, limit)
	if err != nil {
		return nil, fmt.Errorf("credential store: reading settlements: %w", err)
	}
	defer rows.Close()
	page := make([]PositionedSettlement, 0, limit)
	for rows.Next() {
		var row PositionedSettlement
		var body string
		if err := rows.Scan(&row.Position, &body); err != nil {
			return nil, fmt.Errorf("credential store: reading a settlement: %w", err)
		}
		row.Body = []byte(body)
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("credential store: reading settlements: %w", err)
	}
	return page, nil
}
