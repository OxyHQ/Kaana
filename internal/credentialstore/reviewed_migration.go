package credentialstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// ReviewedScopedMigrationReport contains schema metadata only. It never exposes
// a connection URL, ciphertext, credentials, principal data or provider payload.
type ReviewedScopedMigrationReport struct {
	Pending                    []string `json:"pending"`
	Checksum                   string   `json:"checksum"`
	ReadOnly                   bool     `json:"readOnly"`
	RecorderAuthorityUnchanged bool     `json:"recorderAuthorityUnchanged"`
	AuthoritySHA256            string   `json:"authoritySha256"`
}

// ReviewedScopedMigration admits only the reviewed additive 0020 migration.
// Inspection uses a read-only transaction. Apply independently repeats the
// ledger proof, checks authority before/after, and commits only after equality.
// It uses the existing dedicated migrator identity; no runtime grants are added.
func (p *Postgres) ReviewedScopedMigration(ctx context.Context, inspectOnly bool) (ReviewedScopedMigrationReport, error) {
	report := ReviewedScopedMigrationReport{Pending: []string{}, Checksum: migrationChecksum(migration0020), ReadOnly: inspectOnly}
	options := pgx.TxOptions{IsoLevel: pgx.Serializable}
	if inspectOnly {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return report, errors.New("credential store: starting reviewed schema transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = tx.QueryRow(ctx, `SELECT current_setting('transaction_read_only')='on'`).Scan(&report.ReadOnly); err != nil || report.ReadOnly != inspectOnly {
		return report, errors.New("credential store: reviewed transaction mode differs")
	}
	rows, err := tx.Query(ctx, `SELECT version,checksum FROM public.kaana_schema_migrations ORDER BY version`)
	if err != nil {
		return report, errors.New("credential store: reading reviewed migration ledger")
	}
	applied := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return report, errors.New("credential store: reading reviewed migration metadata")
		}
		applied[version] = checksum
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, errors.New("credential store: reading reviewed migration metadata")
	}
	known := schemaMigrations()
	for _, migration := range known {
		if migration.version > "0020" {
			// A later reviewed migration has its own reviewed path
			// (ReviewedUnpinMigration); this one neither applies nor
			// requires it, but never admits it under another checksum.
			if checksum, ok := applied[migration.version]; ok && checksum != migrationChecksum(migration.body) {
				return report, fmt.Errorf("credential store: migration %s checksum mismatch", migration.version)
			}
			delete(applied, migration.version)
			continue
		}
		checksum, ok := applied[migration.version]
		if !ok {
			report.Pending = append(report.Pending, migration.version)
		} else if checksum != migrationChecksum(migration.body) {
			return report, fmt.Errorf("credential store: migration %s checksum mismatch", migration.version)
		}
		delete(applied, migration.version)
	}
	if len(applied) != 0 {
		return report, errors.New("credential store: unrecognized migration ledger entries")
	}
	if len(report.Pending) != 0 && !reflect.DeepEqual(report.Pending, []string{"0020"}) {
		return report, errors.New("credential store: only reviewed migration 0020 may be pending")
	}
	before, err := reviewedSchemaAuthority(ctx, tx)
	if err != nil {
		return report, err
	}
	digest := sha256.Sum256([]byte(before))
	report.AuthoritySHA256 = hex.EncodeToString(digest[:])
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT to_regclass('public.scoped_provider_attempt_claims') IS NOT NULL`).Scan(&exists); err != nil {
		return report, errors.New("credential store: checking reviewed claim table")
	}
	if exists != (len(report.Pending) == 0) {
		return report, errors.New("credential store: claim table and migration ledger disagree")
	}
	if exists {
		if err = reviewedClaimTablePrivate(ctx, tx); err != nil {
			return report, err
		}
	}
	if inspectOnly {
		report.RecorderAuthorityUnchanged = true
		return report, nil
	}
	if len(report.Pending) != 0 {
		if err = applyMigration(ctx, tx, "0020", migration0020); err != nil {
			return report, err
		}
	}
	after, err := reviewedSchemaAuthority(ctx, tx)
	if err != nil {
		return report, err
	}
	if before != after {
		return report, errors.New("credential store: reviewed migration changed existing database authority")
	}
	if err = reviewedClaimTablePrivate(ctx, tx); err != nil {
		return report, err
	}
	if err = tx.Commit(ctx); err != nil {
		return report, errors.New("credential store: committing reviewed migration")
	}
	report.RecorderAuthorityUnchanged = true
	return report, nil
}

func reviewedSchemaAuthority(ctx context.Context, tx pgx.Tx) (string, error) {
	// Existing relation owners/ACLs and role memberships are compared exactly.
	// The new storage table is excluded; its lack of app visibility is separate.
	const query = `SELECT jsonb_build_object(
 'relations',(SELECT jsonb_agg(jsonb_build_array(c.oid,c.relowner,c.relacl) ORDER BY c.oid) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname<>'scoped_provider_attempt_claims' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_index i WHERE i.indexrelid=c.oid AND i.indrelid=to_regclass('public.scoped_provider_attempt_claims'))),
 'schemas',(SELECT jsonb_agg(jsonb_build_array(oid,nspowner,nspacl) ORDER BY oid) FROM pg_catalog.pg_namespace WHERE nspname='public'),
 'defaultPrivileges',(SELECT jsonb_agg(jsonb_build_array(oid,defaclrole,defaclnamespace,defaclobjtype,defaclacl) ORDER BY oid) FROM pg_catalog.pg_default_acl),
 'roles',(SELECT jsonb_agg(jsonb_build_array(oid,rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls,rolconnlimit) ORDER BY oid) FROM pg_catalog.pg_roles),
 'memberships',(SELECT jsonb_agg(jsonb_build_array(roleid,member,grantor,admin_option,inherit_option,set_option) ORDER BY roleid,member,grantor) FROM pg_catalog.pg_auth_members),
 'functions',(SELECT jsonb_agg(jsonb_build_array(p.oid,p.proowner,p.proacl,p.prosecdef,p.proconfig) ORDER BY p.oid) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'))::text`
	var result string
	if err := tx.QueryRow(ctx, query).Scan(&result); err != nil {
		return "", errors.New("credential store: reading existing schema authority")
	}
	var secured bool
	if err := tx.QueryRow(ctx, `SELECT prosecdef AND proconfig=ARRAY['search_path=pg_catalog']::text[] FROM pg_catalog.pg_proc WHERE oid='public.kaana_record_provider_credential_attempt(text,text,integer,text,text,text,text,timestamp with time zone,timestamp with time zone)'::regprocedure`).Scan(&secured); err != nil || !secured {
		return "", errors.New("credential store: recorder security configuration differs")
	}
	return result, nil
}

func reviewedClaimTablePrivate(ctx context.Context, tx pgx.Tx) error {
	var expanded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(c.relacl,pg_catalog.acldefault('r',c.relowner))) a WHERE c.oid='public.scoped_provider_attempt_claims'::regclass AND a.grantee<>c.relowner)`).Scan(&expanded); err != nil {
		return errors.New("credential store: checking claim table authority")
	}
	if expanded {
		return errors.New("credential store: claim table has non-owner access grants")
	}
	return nil
}

// ReviewedUnpinMigrationReport contains schema metadata only.
type ReviewedUnpinMigrationReport struct {
	Pending  []string `json:"pending"`
	Checksum string   `json:"checksum"`
	ReadOnly bool     `json:"readOnly"`
	// UnpinInstalled is true once kaana_unpin_provider_deployment exists and
	// is executable by kaana_credential_admin and by nobody else.
	UnpinInstalled bool `json:"unpinInstalled"`
}

// ReviewedUnpinMigration admits only the reviewed migration 0021, which adds
// the audited unpin mutation for exact deployment bindings. Every earlier
// migration must already be applied with its exact checksum. Inspection is a
// read-only transaction; apply repeats the proof and commits only after the
// function's grant is exactly the credential-admin role.
func (p *Postgres) ReviewedUnpinMigration(ctx context.Context, inspectOnly bool) (ReviewedUnpinMigrationReport, error) {
	report := ReviewedUnpinMigrationReport{Pending: []string{}, Checksum: migrationChecksum(migration0021), ReadOnly: inspectOnly}
	options := pgx.TxOptions{IsoLevel: pgx.Serializable}
	if inspectOnly {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return report, errors.New("credential store: starting reviewed schema transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT version,checksum FROM public.kaana_schema_migrations ORDER BY version`)
	if err != nil {
		return report, errors.New("credential store: reading reviewed migration ledger")
	}
	applied := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return report, errors.New("credential store: reading reviewed migration metadata")
		}
		applied[version] = checksum
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, errors.New("credential store: reading reviewed migration metadata")
	}
	for _, migration := range schemaMigrations() {
		checksum, ok := applied[migration.version]
		if !ok {
			report.Pending = append(report.Pending, migration.version)
		} else if checksum != migrationChecksum(migration.body) {
			return report, fmt.Errorf("credential store: migration %s checksum mismatch", migration.version)
		}
		delete(applied, migration.version)
	}
	if len(applied) != 0 {
		return report, errors.New("credential store: unrecognized migration ledger entries")
	}
	if len(report.Pending) != 0 && !reflect.DeepEqual(report.Pending, []string{"0021"}) {
		return report, errors.New("credential store: only reviewed migration 0021 may be pending")
	}
	if !inspectOnly && len(report.Pending) != 0 {
		if err = applyMigration(ctx, tx, "0021", migration0021); err != nil {
			return report, err
		}
	}
	if inspectOnly && len(report.Pending) != 0 {
		return report, nil
	}
	if err = tx.QueryRow(ctx, `SELECT
		has_function_privilege('kaana_credential_admin', 'public.kaana_unpin_provider_deployment(text,text,text,text,text)', 'EXECUTE')
		AND NOT has_function_privilege('kaana_runtime', 'public.kaana_unpin_provider_deployment(text,text,text,text,text)', 'EXECUTE')`).Scan(&report.UnpinInstalled); err != nil {
		return report, errors.New("credential store: checking the reviewed unpin grant")
	}
	if !report.UnpinInstalled {
		return report, errors.New("credential store: the unpin mutation is not granted exactly to credential administration")
	}
	if inspectOnly {
		return report, nil
	}
	if err = tx.Commit(ctx); err != nil {
		return report, errors.New("credential store: committing reviewed migration")
	}
	return report, nil
}
