package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RetentionTable names one table the retention purge may shrink. The set is
// closed: each maps to its own SECURITY DEFINER function, so no table name is
// ever interpolated into SQL.
type RetentionTable string

const (
	RetentionCostEvents     RetentionTable = "provider_cost_events"
	RetentionAttemptEvents  RetentionTable = "provider_credential_attempt_events"
	RetentionSettlementsOut RetentionTable = "inference_settlement_outbox"
)

// RetentionTables is every purgeable table, in the order a purge runs them.
func RetentionTables() []RetentionTable {
	return []RetentionTable{RetentionCostEvents, RetentionAttemptEvents, RetentionSettlementsOut}
}

// MinimumRetention is the shortest cutoff age the database accepts; the SQL
// refuses anything newer whatever the caller passes.
const MinimumRetention = 30 * 24 * time.Hour

// MaxPurgeBatch is the most rows one call touches.
const MaxPurgeBatch = 10000

// PurgeRetention counts (dryRun) or deletes up to limit rows of table that are
// older than the cutoff, and returns how many. For the settlement outbox only
// DELIVERED rows qualify, by delivery time: an undelivered settlement is the
// only copy of a charge and is never purged. Call it again until it returns
// less than limit.
func (p *Postgres) PurgeRetention(ctx context.Context, table RetentionTable, olderThan time.Time, limit int, dryRun bool) (int64, error) {
	var function string
	switch table {
	case RetentionCostEvents:
		function = "kaana_purge_provider_cost_events"
	case RetentionAttemptEvents:
		function = "kaana_purge_provider_credential_attempt_events"
	case RetentionSettlementsOut:
		function = "kaana_purge_delivered_settlements"
	default:
		return 0, errors.New("credential store: not a purgeable table")
	}
	if limit < 1 || limit > MaxPurgeBatch {
		return 0, fmt.Errorf("credential store: a purge batch is 1 to %d rows", MaxPurgeBatch)
	}
	var affected int64
	if err := p.pool.QueryRow(ctx, "SELECT "+function+"($1,$2,$3)", olderThan, limit, dryRun).Scan(&affected); err != nil {
		return 0, fmt.Errorf("credential store: retention purge of %s: %w", table, err)
	}
	return affected, nil
}
