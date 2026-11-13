package nativeadmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/jackc/pgx/v5"
)

func completionDigest(report contract.UsageReport, events []providercost.Event) (string, error) {
	raw, err := json.Marshal(struct {
		Report contract.UsageReport
		Events []providercost.Event
	}{report, events})
	if err != nil {
		return "", err
	}
	return digest(raw), nil
}

// StageCompletion durably accepts content-free provider evidence before a public
// terminal is emitted. A process can die after this commit: ReplayCompletions
// finishes the same admission and never contacts an inference provider.
func (s *Store) StageCompletion(ctx context.Context, id string, report contract.UsageReport, events []providercost.Event) error {
	if err := report.Validate(); err != nil {
		return err
	}
	fingerprint, err := completionDigest(report, events)
	if err != nil {
		return err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	eventBody, err := json.Marshal(events)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	admission, err := readAdmission(ctx, tx, id)
	if err != nil {
		return err
	}
	if report.RequestID != admission.RequestID || !sameAttribution(admission.Attribution(), report.Attribution) || !sameGeneration(admission.GenerationID, report.GenerationID) {
		return ErrConflict
	}
	for _, event := range events {
		if event.RequestID != admission.RequestID {
			return ErrConflict
		}
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT digest FROM kaana_admission.completions WHERE admission_id=$1`, id).Scan(&previous)
	if err == nil {
		if previous != fingerprint {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// A replay after direct settlement must agree with that immutable outcome.
	err = tx.QueryRow(ctx, `SELECT digest FROM kaana_admission.settlements WHERE admission_id=$1`, id).Scan(&previous)
	if err == nil {
		if previous != fingerprint {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.completions(admission_id,digest,report,provider_events) VALUES($1,$2,$3,$4)`, id, fingerprint, body, eventBody); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE kaana_admission.admissions SET state='pending_settlement',generation_id=$2 WHERE id=$1`, id, report.GenerationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReplayCompletions applies a bounded batch of pending immutable evidence.
// Multiple workers may see the same row; Settle serializes on its admission and
// rejects changed evidence. One bad row does not block the rest of the batch.
func (s *Store) ReplayCompletions(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidRequest
	}
	rows, err := s.pool.Query(ctx, `SELECT c.admission_id,c.report,c.provider_events FROM kaana_admission.completions c LEFT JOIN kaana_admission.settlements s ON s.admission_id=c.admission_id WHERE s.admission_id IS NULL ORDER BY c.created_at,c.admission_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type completion struct {
		id     string
		report contract.UsageReport
		events []providercost.Event
	}
	batch := make([]completion, 0, limit)
	for rows.Next() {
		var item completion
		var report, events []byte
		if err = rows.Scan(&item.id, &report, &events); err != nil {
			rows.Close()
			return 0, err
		}
		if err = json.Unmarshal(report, &item.report); err != nil {
			rows.Close()
			return 0, err
		}
		if err = json.Unmarshal(events, &item.events); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	completed := 0
	var firstError error
	for _, item := range batch {
		if err = s.Settle(ctx, item.id, item.report, item.events); err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("nativeadmission: replay completion: %w", err)
			}
			continue
		}
		completed++
	}
	return completed, firstError
}
