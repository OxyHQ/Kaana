package nativeadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PolicyFile struct {
	SchemaVersion int      `json:"schemaVersion"`
	Policies      []Policy `json:"policies"`
}

// LoadPolicyFile accepts only an explicitly reviewed content-addressed file.
// It never derives a product enrollment from branding or an identity owner.
func LoadPolicyFile(path, expectedSHA256 string) ([]Policy, error) {
	if path == "" || len(expectedSHA256) != 64 {
		return nil, errors.New("nativeadmission: policy path and reviewed SHA256 are required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("nativeadmission: policy file unavailable")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 || digest(raw) != expectedSHA256 {
		return nil, errors.New("nativeadmission: policy file does not match reviewed SHA256")
	}
	var file PolicyFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&file); err != nil {
		return nil, errors.New("nativeadmission: policy file is malformed")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("nativeadmission: trailing policy data")
	}
	if file.SchemaVersion != 1 || len(file.Policies) == 0 {
		return nil, errors.New("nativeadmission: explicit versioned policies are required")
	}
	if _, err = NewPlanner(file.Policies, nil); err != nil {
		return nil, err
	}
	return file.Policies, nil
}

// OpenRuntime uses a separate pool restricted to the admission role. Startup
// checks the installed schema; serving never runs a migration or enrollment.
func OpenRuntime(ctx context.Context, databaseURL string) (*Store, func(), error) {
	config, err := credentialstore.VerifiedPostgresConfig(databaseURL)
	if err != nil {
		return nil, nil, errors.New("nativeadmission: invalid database configuration")
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE kaana_admission_runtime`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, nil, errors.New("nativeadmission: runtime database unavailable")
	}
	var valid bool
	err = pool.QueryRow(ctx, `SELECT current_user='kaana_admission_runtime' AND has_table_privilege(current_user,'kaana_admission.admissions','INSERT') AND NOT has_table_privilege(current_user,'kaana_admission.policy_versions','INSERT')`).Scan(&valid)
	if err != nil || !valid {
		pool.Close()
		return nil, nil, errors.New("nativeadmission: runtime schema or role is not ready")
	}
	return NewStore(pool), pool.Close, nil
}

// OpenAdministration selects only the explicit operator role. Its login and
// role memberships are provisioned separately from serving credentials.
func OpenAdministration(ctx context.Context, databaseURL string, migrate bool) (*Store, func(), error) {
	config, err := credentialstore.VerifiedPostgresConfig(databaseURL)
	if err != nil {
		return nil, nil, err
	}
	role := "SET ROLE kaana_admission_admin"
	if migrate {
		role = "SET ROLE kaana_migrator"
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error { _, err := conn.Exec(ctx, role); return err }
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, nil, errors.New("nativeadmission: administration database unavailable")
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, errors.New("nativeadmission: administration role unavailable")
	}
	return NewStore(pool), pool.Close, nil
}
