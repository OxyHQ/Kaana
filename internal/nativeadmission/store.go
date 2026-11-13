package nativeadmission

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0001_admissions.sql
var admissionMigration string

var (
	ErrCapacity = errors.New("nativeadmission: capacity exhausted")
	ErrDisabled = errors.New("nativeadmission: enrollment is disabled or unfenced")
	ErrReplay   = errors.New("nativeadmission: request already admitted")
	ErrConflict = errors.New("nativeadmission: idempotency conflict")
	ErrLegacy   = errors.New("nativeadmission: idempotency key belongs to the previous authority")
	ErrNotFound = errors.New("nativeadmission: generation not found")
)

// Store's pool has the admission role; migration/enrollment use a distinct admin
// pool. Neither role can read provider secrets or mutate customer balances.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("nativeadmission: system entropy unavailable")
	}
	return prefix + hex.EncodeToString(b[:])
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// Migrate is explicit and never called by the inference server's constructor.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("nativeadmission: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kaana-native-admission-migrations'))`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.kaana_admission_migrations(version TEXT PRIMARY KEY,checksum TEXT NOT NULL); REVOKE ALL ON public.kaana_admission_migrations FROM PUBLIC`); err != nil {
		return err
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT checksum FROM public.kaana_admission_migrations WHERE version='0001'`).Scan(&previous)
	if err == nil {
		if previous != digest([]byte(admissionMigration)) {
			return errors.New("nativeadmission: migration checksum mismatch")
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("nativeadmission: migration ledger: %w", err)
	}
	if _, err = tx.Exec(ctx, admissionMigration); err != nil {
		return fmt.Errorf("nativeadmission: applying migration: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.kaana_admission_migrations VALUES('0001',$1)`, digest([]byte(admissionMigration))); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RegisterPolicy stores an immutable reviewed version. Enrollment remains off:
// importing the legacy key fence and activation is a separate reviewed action.
func (s *Store) RegisterPolicy(ctx context.Context, p Policy) error {
	if err := p.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.enrollments(id,application_id,environment) VALUES($1,$2,$3) ON CONFLICT(application_id,environment) DO NOTHING`, newID("ken_"), p.ApplicationID, p.Environment); err != nil {
		return err
	}
	var enrollment string
	if err = tx.QueryRow(ctx, `SELECT id FROM kaana_admission.enrollments WHERE application_id=$1 AND environment=$2 FOR UPDATE`, p.ApplicationID, p.Environment).Scan(&enrollment); err != nil {
		return err
	}
	var old string
	err = tx.QueryRow(ctx, `SELECT digest FROM kaana_admission.policy_versions WHERE policy_key=$1 AND version=$2`, p.ID, p.Version).Scan(&old)
	if err == nil {
		if old != policyDigest(p) {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.policy_versions(id,enrollment_id,policy_key,version,digest,document) VALUES($1,$2,$3,$4,$5,$6)`, newID("kpv_"), enrollment, p.ID, p.Version, policyDigest(p), raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type AdmissionRequest struct {
	RequestID      contract.RequestID
	Principal      contract.IdentityPrincipal
	UserID         *contract.UserID
	Operation      contract.GrantOperation
	BodyDigest     string
	IdempotencyKey *contract.IdempotencyKey
	Plan           Plan
}

type Admission struct {
	ID         string
	RequestID  contract.RequestID
	Principal  contract.IdentityPrincipal
	UserID     *contract.UserID
	Operation  contract.GrantOperation
	Plan       Plan
	AdmittedAt time.Time
	LeaseUntil time.Time
}

// Admit claims both idempotency and distributed capacity in a single locked
// transaction. Only a fresh result permits execution; every replay is explicit.
func (s *Store) Admit(ctx context.Context, request AdmissionRequest) (*Admission, error) {
	if err := request.Principal.Validate(); err != nil {
		return nil, err
	}
	if request.RequestID == "" || !request.Operation.Valid() || !contract.ValidSHA256Hex(request.BodyDigest) {
		return nil, ErrInvalidRequest
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT kaana_admission.lock_enrollment($1,$2)`, request.Principal.ApplicationID, request.Principal.Environment); err != nil {
		return nil, err
	}
	var enrollment, policyID string
	var activePolicy *string
	var enabled, fenced bool
	var activation *time.Time
	var now time.Time
	var policyBytes []byte
	err = tx.QueryRow(ctx, `SELECT e.id,e.enabled,e.legacy_fence_complete,e.activation_at,p.id,p.document,e.active_policy_id,clock_timestamp() FROM kaana_admission.enrollments e JOIN kaana_admission.policy_versions p ON p.enrollment_id=e.id WHERE e.application_id=$1 AND e.environment=$2 AND p.policy_key=$3 AND p.version=$4 AND p.digest=$5`, request.Principal.ApplicationID, request.Principal.Environment, request.Plan.PolicyID, request.Plan.PolicyVersion, request.Plan.PolicyDigest).Scan(&enrollment, &enabled, &fenced, &activation, &policyID, &policyBytes, &activePolicy, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotEnrolled
	}
	if err != nil {
		return nil, err
	}
	if !enabled || !fenced || activation == nil || now.Before(*activation) {
		return nil, ErrDisabled
	}
	if activePolicy == nil || *activePolicy != policyID {
		return nil, ErrNotEnrolled
	}
	var policy Policy
	if err = json.Unmarshal(policyBytes, &policy); err != nil {
		return nil, err
	}
	if !policy.matches(request.Principal) || policy.MaxConcurrent != request.Plan.MaxConcurrent || policy.MaxPerUTCDay != request.Plan.MaxPerUTCDay || !policy.admitsExecution(request.Operation, request.Plan) {
		return nil, ErrNotEnrolled
	}
	scope, key := idempotencyIdentity(request.Principal.CredentialID, request.IdempotencyKey, request.RequestID)
	{
		var existingID, body string
		err = tx.QueryRow(ctx, `SELECT id,body_digest FROM kaana_admission.admissions WHERE idempotency_scope=$1 AND idempotency_key=$2`, scope, key).Scan(&existingID, &body)
		if err == nil {
			if body != request.BodyDigest {
				return nil, ErrConflict
			}
			return nil, ErrReplay
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var legacyBody *string
		err = tx.QueryRow(ctx, `SELECT body_digest FROM kaana_admission.legacy_fences WHERE idempotency_scope=$1 AND idempotency_key=$2`, scope, key).Scan(&legacyBody)
		if err == nil {
			if legacyBody != nil && *legacyBody != request.BodyDigest {
				return nil, ErrConflict
			}
			return nil, ErrLegacy
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM kaana_admission.admissions WHERE enrollment_id=$1 AND state='executing' AND lease_until>$2`, enrollment, now).Scan(&active); err != nil {
		return nil, err
	}
	if active >= policy.MaxConcurrent {
		return nil, ErrCapacity
	}
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.daily_windows(enrollment_id,utc_day,admitted) VALUES($1,($2::timestamptz AT TIME ZONE 'UTC')::date,0) ON CONFLICT DO NOTHING`, enrollment, now); err != nil {
		return nil, err
	}
	result, err := tx.Exec(ctx, `UPDATE kaana_admission.daily_windows SET admitted=admitted+1 WHERE enrollment_id=$1 AND utc_day=($2::timestamptz AT TIME ZONE 'UTC')::date AND admitted<$3`, enrollment, now, policy.MaxPerUTCDay)
	if err != nil {
		return nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, ErrCapacity
	}
	a := &Admission{ID: newID("kad_"), RequestID: request.RequestID, Principal: request.Principal, UserID: request.UserID, Operation: request.Operation, Plan: request.Plan, AdmittedAt: now, LeaseUntil: now.Add(request.Plan.ExecutionTimeout)}
	principal, err := json.Marshal(a.Principal)
	if err != nil {
		return nil, err
	}
	plan, err := json.Marshal(a.Plan)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.admissions(id,enrollment_id,policy_id,request_id,principal,delegated_user_id,operation,body_digest,idempotency_scope,idempotency_key,plan,state,admitted_at,lease_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'executing',$12,$13)`, a.ID, enrollment, policyID, a.RequestID, principal, a.UserID, a.Operation, request.BodyDigest, scope, key, plan, now, a.LeaseUntil); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// Oxy's durable namespace is credential+key, not user attribution or a hold TTL.
func idempotencyIdentity(credential contract.CredentialID, key *contract.IdempotencyKey, request contract.RequestID) (string, string) {
	if key == nil {
		return "request", string(request)
	}
	return "credential:" + string(credential), string(*key)
}
