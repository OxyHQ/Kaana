package nativeadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/jackc/pgx/v5"
)

// Oxy serializes Date.toISOString(): exact UTC milliseconds. This precision
// survives PostgreSQL timestamp storage without changing epoch or chronology.
const legacyManifestTimeLayout = "2006-01-02T15:04:05.000Z"

type LegacyManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Authority     string `json:"authority"`
	FenceID       string `json:"fenceId"`
	ActivatedAt   string `json:"activatedAt"`
	ExportedAt    string `json:"exportedAt"`
	Population    struct {
		ApplicationID  contract.ApplicationID  `json:"applicationId"`
		OwnerAccountID contract.AccountID      `json:"ownerAccountId"`
		Environment    contract.Environment    `json:"environment"`
		CredentialKind string                  `json:"credentialKind"`
		CredentialIDs  []contract.CredentialID `json:"credentialIds"`
	} `json:"population"`
	Unresolved struct {
		Admissions int `json:"admissions"`
		Grants     int `json:"grants"`
		Holds      int `json:"holds"`
	} `json:"unresolved"`
	DailyUsage LegacyDailyUsage `json:"dailyUsage"`
	Entries    []LegacyEntry    `json:"entries"`
	SHA256     string           `json:"sha256"`
}
type LegacyDailyUsage struct {
	Scope                string `json:"scope"`
	UTCDay               string `json:"utcDay"`
	Admitted             int64  `json:"admitted"`
	MaxRequestsPerUTCDay int    `json:"maxRequestsPerUtcDay"`
}

type LegacyEntry struct {
	CredentialID      contract.CredentialID `json:"credentialId"`
	KeyKind           string                `json:"keyKind"`
	Key               string                `json:"key"`
	LegacyAdmissionID string                `json:"legacyAdmissionId"`
	RequestID         contract.RequestID    `json:"requestId"`
	BodyDigest        *string               `json:"bodyDigest"`
	DigestDomain      string                `json:"digestDomain"`
}

// ImportReview is external review evidence, not data trusted from its own file.
// Policy must already be registered; its full immutable digest is compared.
type ImportReview struct {
	PolicyDigest   string
	ManifestSHA256 string
	FenceID        string
	ActivatedAt    string
	Policy         Policy
}
type ImportResult struct {
	PolicyID       string `json:"policyId"`
	PolicyVersion  int    `json:"policyVersion"`
	PolicyDigest   string `json:"policyDigest"`
	ManifestSHA256 string `json:"manifestSha256"`
	Entries        int    `json:"entries"`
}

func DecodeLegacyManifest(raw []byte, review ImportReview, now time.Time) (LegacyManifest, error) {
	var manifest LegacyManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, errors.New("nativeadmission: malformed legacy manifest")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, errors.New("nativeadmission: trailing manifest data")
	}
	var payload map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&payload); err != nil {
		return manifest, err
	}
	if !hasFields(payload, "schemaVersion", "authority", "fenceId", "activatedAt", "exportedAt", "population", "unresolved", "dailyUsage", "entries", "sha256") {
		return manifest, ErrInvalidRequest
	}
	population, ok := payload["population"].(map[string]any)
	if !ok || !hasFields(population, "applicationId", "ownerAccountId", "environment", "credentialKind", "credentialIds") {
		return manifest, ErrInvalidRequest
	}
	unresolved, ok := payload["unresolved"].(map[string]any)
	if !ok || !hasFields(unresolved, "admissions", "grants", "holds") {
		return manifest, ErrInvalidRequest
	}
	daily, ok := payload["dailyUsage"].(map[string]any)
	if !ok || !hasFields(daily, "scope", "utcDay", "admitted", "maxRequestsPerUtcDay") {
		return manifest, ErrInvalidRequest
	}
	entries, ok := payload["entries"].([]any)
	if !ok {
		return manifest, ErrInvalidRequest
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok || !hasFields(entry, "credentialId", "keyKind", "key", "legacyAdmissionId", "requestId", "bodyDigest", "digestDomain") {
			return manifest, ErrInvalidRequest
		}
	}
	delete(payload, "sha256")
	canonical, err := canonicalManifest(payload)
	if err != nil {
		return manifest, err
	}
	if len(review.ManifestSHA256) != 64 || digest(canonical) != review.ManifestSHA256 || manifest.SHA256 != review.ManifestSHA256 {
		return manifest, errors.New("nativeadmission: reviewed manifest hash mismatch")
	}
	if err = review.Policy.validate(); err != nil {
		return manifest, err
	}
	p := review.Policy
	if review.PolicyDigest != policyDigest(p) {
		return manifest, errors.New("nativeadmission: reviewed policy digest mismatch")
	}
	if manifest.SchemaVersion != 2 || manifest.Authority != "oxy" || manifest.FenceID == "" || manifest.FenceID != review.FenceID || manifest.ActivatedAt != review.ActivatedAt || manifest.Population.CredentialKind != "application" || p.Authority != contract.AuthorityFirstParty || p.Lane != contract.IdentityLaneService || p.ApplicationID != manifest.Population.ApplicationID || p.OwnerAccountID != manifest.Population.OwnerAccountID || p.Environment != manifest.Population.Environment {
		return manifest, errors.New("nativeadmission: reviewed population or fence epoch mismatch")
	}
	activated, err := time.Parse(time.RFC3339Nano, manifest.ActivatedAt)
	if err != nil || activated.UTC().Format(legacyManifestTimeLayout) != manifest.ActivatedAt {
		return manifest, errors.New("nativeadmission: fence timestamp must be canonical UTC milliseconds")
	}
	exported, err := time.Parse(time.RFC3339Nano, manifest.ExportedAt)
	if err != nil || activated.After(exported) || exported.After(now) || exported.UTC().Format(legacyManifestTimeLayout) != manifest.ExportedAt {
		return manifest, errors.New("nativeadmission: invalid fence chronology")
	}
	dailyUsage := manifest.DailyUsage
	if dailyUsage.Scope != "application_environment" || dailyUsage.UTCDay != exported.UTC().Format("2006-01-02") || dailyUsage.Admitted < 0 || dailyUsage.Admitted > 9007199254740991 || dailyUsage.MaxRequestsPerUTCDay <= 0 || dailyUsage.MaxRequestsPerUTCDay != p.MaxPerUTCDay {
		return manifest, errors.New("nativeadmission: invalid reviewed application daily usage")
	}
	if manifest.Unresolved.Admissions != 0 || manifest.Unresolved.Grants != 0 || manifest.Unresolved.Holds != 0 {
		return manifest, errors.New("nativeadmission: legacy admissions have not drained")
	}
	if manifest.Entries == nil || len(manifest.Population.CredentialIDs) == 0 {
		return manifest, ErrInvalidRequest
	}
	for i, id := range manifest.Population.CredentialIDs {
		if id == "" || i > 0 && utf16Compare(string(manifest.Population.CredentialIDs[i-1]), string(id)) >= 0 {
			return manifest, errors.New("nativeadmission: invalid live credential roster")
		}
	}
	for _, id := range p.CredentialIDs {
		if !slices.Contains(manifest.Population.CredentialIDs, id) {
			return manifest, errors.New("nativeadmission: policy credential is outside fenced live roster")
		}
	}
	previous := ""
	for _, entry := range manifest.Entries {
		if entry.CredentialID == "" || entry.Key == "" || entry.RequestID == "" || entry.LegacyAdmissionID == "" || entry.BodyDigest != nil || entry.DigestDomain != "unknown" || (entry.KeyKind != "idempotency_key" && entry.KeyKind != "request_id") || entry.KeyKind == "request_id" && entry.Key != string(entry.RequestID) {
			return manifest, errors.New("nativeadmission: invalid legacy tombstone")
		}
		order, _ := canonicalManifest([]any{string(entry.CredentialID), entry.KeyKind, entry.Key})
		if previous != "" && utf16Compare(previous, string(order)) >= 0 {
			return manifest, errors.New("nativeadmission: duplicate or unordered legacy tombstones")
		}
		previous = string(order)
	}
	return manifest, nil
}

// ImportLegacyFence atomically installs all permanent tombstones and activates
// exactly the reviewed policy. The inference role cannot call this transaction.
func (s *Store) ImportLegacyFence(ctx context.Context, raw []byte, review ImportReview) (ImportResult, error) {
	manifest, err := DecodeLegacyManifest(raw, review, time.Now())
	if err != nil {
		return ImportResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var enrollment, policyID, storedDigest string
	var previousID, previousDigest, activePolicyKey *string
	var activeVersion *int
	var previousActivation *time.Time
	err = tx.QueryRow(ctx, `SELECT e.id,p.id,p.digest,e.legacy_fence_id,e.legacy_fence_digest,e.legacy_fence_activated_at,active.policy_key,active.version FROM kaana_admission.enrollments e JOIN kaana_admission.policy_versions p ON p.enrollment_id=e.id LEFT JOIN kaana_admission.policy_versions active ON active.id=e.active_policy_id WHERE e.application_id=$1 AND e.environment=$2 AND p.policy_key=$3 AND p.version=$4 FOR UPDATE OF e`, review.Policy.ApplicationID, review.Policy.Environment, review.Policy.ID, review.Policy.Version).Scan(&enrollment, &policyID, &storedDigest, &previousID, &previousDigest, &previousActivation, &activePolicyKey, &activeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImportResult{}, ErrNotEnrolled
	}
	if err != nil {
		return ImportResult{}, err
	}
	activated, _ := time.Parse(time.RFC3339Nano, manifest.ActivatedAt)
	exported, _ := time.Parse(time.RFC3339Nano, manifest.ExportedAt)
	if storedDigest != policyDigest(review.Policy) {
		return ImportResult{}, ErrConflict
	}
	if previousID != nil {
		if *previousID != manifest.FenceID || previousActivation == nil || !previousActivation.Equal(activated) || previousDigest == nil || activePolicyKey == nil || activeVersion == nil || *activePolicyKey != review.Policy.ID || review.Policy.Version < *activeVersion {
			return ImportResult{}, ErrConflict
		}
		if *previousDigest != manifest.SHA256 && review.Policy.Version <= *activeVersion {
			return ImportResult{}, ErrConflict
		}
		var latest *time.Time
		if err = tx.QueryRow(ctx, `SELECT max(exported_at) FROM kaana_admission.fence_imports WHERE enrollment_id=$1`, enrollment).Scan(&latest); err != nil {
			return ImportResult{}, err
		}
		if latest == nil || exported.Before(*latest) {
			return ImportResult{}, ErrConflict
		}
	}
	// A completed exact replay is a read-only acknowledgement, even on a later
	// UTC day. It must not reactivate disabled enrollment or reset today's quota.
	if previousDigest != nil && *previousDigest == manifest.SHA256 && activeVersion != nil && *activeVersion == review.Policy.Version {
		var recorded bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM kaana_admission.fence_imports WHERE enrollment_id=$1 AND manifest_digest=$2 AND daily_usage IS NOT NULL)`, enrollment, manifest.SHA256).Scan(&recorded); err != nil {
			return ImportResult{}, err
		}
		if recorded {
			if err = tx.Commit(ctx); err != nil {
				return ImportResult{}, err
			}
			return ImportResult{review.Policy.ID, review.Policy.Version, storedDigest, manifest.SHA256, len(manifest.Entries)}, nil
		}
	}
	var databaseDay string
	if err = tx.QueryRow(ctx, `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD')`).Scan(&databaseDay); err != nil {
		return ImportResult{}, err
	}
	if manifest.DailyUsage.UTCDay != databaseDay {
		return ImportResult{}, errors.New("nativeadmission: fresh UTC-day usage snapshot required")
	}
	coverage := make(map[string]LegacyEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		scope := "request"
		if entry.KeyKind == "idempotency_key" {
			scope = "credential:" + string(entry.CredentialID)
		}
		index := scope + "\x00" + entry.Key
		if old, ok := coverage[index]; ok && (old.LegacyAdmissionID != entry.LegacyAdmissionID || old.RequestID != entry.RequestID) {
			return ImportResult{}, ErrConflict
		}
		coverage[index] = entry
	}
	rows, err := tx.Query(ctx, `SELECT idempotency_scope,idempotency_key,legacy_admission_id,legacy_request_id FROM kaana_admission.legacy_fences WHERE enrollment_id=$1`, enrollment)
	if err != nil {
		return ImportResult{}, err
	}
	for rows.Next() {
		var scope, key, legacy string
		var request *string
		if err = rows.Scan(&scope, &key, &legacy, &request); err != nil {
			rows.Close()
			return ImportResult{}, err
		}
		entry, ok := coverage[scope+"\x00"+key]
		if !ok || request == nil || legacy != entry.LegacyAdmissionID || *request != string(entry.RequestID) {
			rows.Close()
			return ImportResult{}, ErrConflict
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return ImportResult{}, err
	}
	rows.Close()

	for _, entry := range manifest.Entries {
		scope, key := "request", entry.Key
		if entry.KeyKind == "idempotency_key" {
			scope = "credential:" + string(entry.CredentialID)
		}
		var conflict bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM kaana_admission.admissions WHERE idempotency_scope=$1 AND idempotency_key=$2 OR request_id=$3)`, scope, key, entry.RequestID).Scan(&conflict); err != nil {
			return ImportResult{}, err
		}
		if conflict {
			return ImportResult{}, ErrConflict
		}
		if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.legacy_fences(id,enrollment_id,idempotency_scope,idempotency_key,body_digest,legacy_admission_id,legacy_request_id) VALUES($1,$2,$3,$4,NULL,$5,$6) ON CONFLICT(idempotency_scope,idempotency_key) DO NOTHING`, newID("kif_"), enrollment, scope, key, entry.LegacyAdmissionID, entry.RequestID); err != nil {
			return ImportResult{}, err
		}
		var own, legacy string
		var request *string
		var body *string
		if err = tx.QueryRow(ctx, `SELECT enrollment_id,legacy_admission_id,legacy_request_id,body_digest FROM kaana_admission.legacy_fences WHERE idempotency_scope=$1 AND idempotency_key=$2`, scope, key).Scan(&own, &legacy, &request, &body); err != nil {
			return ImportResult{}, err
		}
		if own != enrollment || legacy != entry.LegacyAdmissionID || request == nil || *request != string(entry.RequestID) || body != nil {
			return ImportResult{}, ErrConflict
		}
	}
	if err = importLegacyDailyUsage(ctx, tx, enrollment, manifest); err != nil {
		return ImportResult{}, err
	}
	dailyJSON, _ := json.Marshal(manifest.DailyUsage)
	roster, _ := json.Marshal(manifest.Population.CredentialIDs)
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.fence_imports(id,enrollment_id,policy_id,manifest_digest,fence_id,activated_at,exported_at,credential_ids,entry_count,daily_usage) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(enrollment_id,manifest_digest) DO NOTHING`, newID("kim_"), enrollment, policyID, manifest.SHA256, manifest.FenceID, activated, exported, roster, len(manifest.Entries), dailyJSON); err != nil {
		return ImportResult{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE kaana_admission.enrollments SET enabled=$2,active_policy_id=$3,activation_at=COALESCE(activation_at,clock_timestamp()),legacy_fence_complete=true,legacy_fence_id=$4,legacy_fence_digest=$5,legacy_fence_activated_at=$6 WHERE id=$1`, enrollment, review.Policy.Enabled, policyID, manifest.FenceID, manifest.SHA256, manifest.ActivatedAt); err != nil {
		return ImportResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{review.Policy.ID, review.Policy.Version, storedDigest, manifest.SHA256, len(manifest.Entries)}, nil
}

func hasFields(value map[string]any, fields ...string) bool {
	for _, field := range fields {
		if _, ok := value[field]; !ok {
			return false
		}
	}
	return true
}

// PolicyDigest is the immutable policy fingerprint used in review and storage.
func PolicyDigest(p Policy) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	return policyDigest(p), nil
}

// The caller owns the same enrollment row lock as Admit. A refresh changes
// only the legacy component; the combined counter retains native admissions.
func importLegacyDailyUsage(ctx context.Context, tx pgx.Tx, enrollment string, manifest LegacyManifest) error {
	daily := manifest.DailyUsage
	var previous int64
	var previousExport time.Time
	err := tx.QueryRow(ctx, `SELECT admitted,exported_at FROM kaana_admission.legacy_daily_usage WHERE enrollment_id=$1 AND utc_day=$2::date FOR UPDATE`, enrollment, daily.UTCDay).Scan(&previous, &previousExport)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	exported, _ := time.Parse(time.RFC3339Nano, manifest.ExportedAt)
	if daily.Admitted < previous || !previousExport.IsZero() && exported.Before(previousExport) {
		return ErrConflict
	}
	delta := daily.Admitted - previous
	if _, err = tx.Exec(ctx, `INSERT INTO kaana_admission.daily_windows(enrollment_id,utc_day,admitted) VALUES($1,$2::date,$3) ON CONFLICT(enrollment_id,utc_day) DO UPDATE SET admitted=kaana_admission.daily_windows.admitted+EXCLUDED.admitted`, enrollment, daily.UTCDay, delta); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO kaana_admission.legacy_daily_usage(enrollment_id,utc_day,admitted,max_requests_per_utc_day,manifest_digest,exported_at) VALUES($1,$2::date,$3,$4,$5,$6) ON CONFLICT(enrollment_id,utc_day) DO UPDATE SET admitted=EXCLUDED.admitted,max_requests_per_utc_day=EXCLUDED.max_requests_per_utc_day,manifest_digest=EXCLUDED.manifest_digest,exported_at=EXCLUDED.exported_at`, enrollment, daily.UTCDay, daily.Admitted, daily.MaxRequestsPerUTCDay, manifest.SHA256, exported)
	return err
}
