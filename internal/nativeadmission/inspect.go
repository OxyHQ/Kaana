package nativeadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Inspection contains only administrative state, never principals, requests,
// receipt rows, replay keys or policy documents.
type Inspection struct {
	MigrationState      string           `json:"migrationState"`
	EnrollmentPresent   bool             `json:"enrollmentPresent"`
	Registered          bool             `json:"registered"`
	Enabled             bool             `json:"enabled"`
	ActivePolicy        *InspectedPolicy `json:"activePolicy"`
	LegacyFenceComplete bool             `json:"legacyFenceComplete"`
}
type InspectedPolicy struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// OpenInspection uses the existing migration operator (owner of the checksum
// ledger). The connection itself is read-only, as is InspectPolicy's snapshot;
// no new role grant or generic SQL execution surface is needed.
func OpenInspection(ctx context.Context, databaseURL string) (*Store, func(), error) {
	config, err := credentialstore.VerifiedPostgresConfig(databaseURL)
	if err != nil {
		return nil, nil, errors.New("native inspection database configuration unavailable")
	}
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	config.ConnConfig.RuntimeParams["lock_timeout"] = "1000"
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE kaana_migrator`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, nil, errors.New("native inspection database unavailable")
	}
	return NewStore(pool), pool.Close, nil
}

// InspectPolicy verifies the packaged migration ledger and exact selected policy
// in one fixed read-only snapshot. Absence is reported; corrupt or conflicting
// state is refused. It never registers, repairs, disables or activates anything.
func (s *Store) InspectPolicy(ctx context.Context, p Policy) (Inspection, error) {
	if err := p.validate(); err != nil {
		return Inspection{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Inspection{}, errors.New("native inspection snapshot unavailable")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	report, err := inspectPolicy(ctx, tx, p)
	if err != nil {
		return Inspection{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Inspection{}, errors.New("native inspection snapshot refused")
	}
	return report, nil
}
func inspectPolicy(ctx context.Context, tx pgx.Tx, p Policy) (Inspection, error) {
	state, err := inspectMigrations(ctx, tx)
	report := Inspection{MigrationState: state}
	if err != nil || state != "verified" {
		return report, err
	}
	var enrollment string
	var active *string
	err = tx.QueryRow(ctx, `SELECT id,enabled,active_policy_id,legacy_fence_complete FROM kaana_admission.enrollments WHERE application_id=$1 AND environment=$2`, p.ApplicationID, p.Environment).Scan(&enrollment, &report.Enabled, &active, &report.LegacyFenceComplete)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return report, errors.New("native inspection enrollment unavailable")
	}
	report.EnrollmentPresent = err == nil
	var registeredEnrollment, storedDigest string
	var document []byte
	err = tx.QueryRow(ctx, `SELECT enrollment_id,digest,document FROM kaana_admission.policy_versions WHERE policy_key=$1 AND version=$2`, p.ID, p.Version).Scan(&registeredEnrollment, &storedDigest, &document)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return report, errors.New("native inspection policy unavailable")
	}
	if err == nil {
		if !report.EnrollmentPresent || registeredEnrollment != enrollment || storedDigest != policyDigest(p) {
			return report, errors.New("native inspection policy mismatch")
		}
		if _, err = inspectDocument(document, storedDigest); err != nil {
			return report, err
		}
		report.Registered = true
	}
	if active != nil {
		var activeDigest string
		var activeVersion int
		var activeDocument []byte
		err = tx.QueryRow(ctx, `SELECT version,digest,document FROM kaana_admission.policy_versions WHERE id=$1 AND enrollment_id=$2`, *active, enrollment).Scan(&activeVersion, &activeDigest, &activeDocument)
		if err != nil {
			return report, errors.New("native inspection active policy unavailable")
		}
		activePolicy, err := inspectDocument(activeDocument, activeDigest)
		if err != nil {
			return report, err
		}
		if activePolicy.Version != activeVersion || activePolicy.ApplicationID != p.ApplicationID || activePolicy.Environment != p.Environment {
			return report, errors.New("native inspection active policy mismatch")
		}
		report.ActivePolicy = &InspectedPolicy{Version: activeVersion, Digest: activeDigest}
	}
	return report, nil
}
func inspectDocument(document []byte, expectedDigest string) (Policy, error) {
	var stored Policy
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return stored, errors.New("native inspection policy document invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return stored, errors.New("native inspection policy document invalid")
	}
	if stored.validate() != nil || policyDigest(stored) != expectedDigest {
		return stored, errors.New("native inspection policy document mismatch")
	}
	return stored, nil
}

// VerifyMigrations checks only the immutable schema ledger, without loading or
// changing any enrollment. An operator needs explicit read access to the ledger.
func (s *Store) VerifyMigrations(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return errors.New("native migration verification unavailable")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	state, err := inspectMigrations(ctx, tx)
	if err != nil {
		return err
	}
	if state != "verified" {
		return errors.New("native migrations must be complete before commercial preparation")
	}
	return tx.Commit(ctx)
}

func inspectMigrations(ctx context.Context, tx pgx.Tx) (string, error) {
	var ledger bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.kaana_admission_migrations') IS NOT NULL`).Scan(&ledger); err != nil {
		return "absent", errors.New("native inspection ledger unavailable")
	}
	if !ledger {
		return "absent", nil
	}
	rows, err := tx.Query(ctx, `SELECT version,checksum FROM public.kaana_admission_migrations ORDER BY version`)
	if err != nil {
		return "absent", errors.New("native inspection ledger unavailable")
	}
	expected := map[string]string{"0001": digest([]byte(admissionMigration)), "0002": digest([]byte(legacyDailyMigration))}
	count := 0
	for rows.Next() {
		var version, checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return "absent", errors.New("native inspection ledger invalid")
		}
		if wanted, ok := expected[version]; !ok || checksum != wanted {
			rows.Close()
			return "absent", errors.New("native inspection migration checksum mismatch")
		}
		count++
	}
	rows.Close()
	if rows.Err() != nil {
		return "absent", errors.New("native inspection ledger unavailable")
	}
	if count != len(expected) {
		return "incomplete", nil
	}
	return "verified", nil
}
