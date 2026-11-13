package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Only PostgreSQL catalog metadata is observed. No application rows, credentials,
// URL, database name or arbitrary SQL are accepted or returned by this command.
const inspectAccessSQL = `SELECT json_build_object(
 'readOnly', current_setting('transaction_read_only')='on',
 'currentRole', current_user, 'sessionRole', session_user,
 'currentRoleSuperuser', (SELECT rolsuper FROM pg_roles WHERE rolname=current_user),
 'currentRoleCreateRole', (SELECT rolcreaterole FROM pg_roles WHERE rolname=current_user),
 'databaseCreate', has_database_privilege(current_user,current_database(),'CREATE'),
 'roles', (SELECT json_agg(json_build_object(
   'name', wanted.name, 'exists', r.oid IS NOT NULL,
   'login', r.rolcanlogin, 'superuser', r.rolsuper, 'createRole', r.rolcreaterole,
   'member', CASE WHEN r.oid IS NULL THEN false ELSE pg_has_role(current_user,r.oid,'MEMBER') END,
   'canSetRole', CASE WHEN r.oid IS NULL THEN false ELSE pg_has_role(current_user,r.oid,'SET') END,
   'directAdminOption', EXISTS(SELECT 1 FROM pg_auth_members m JOIN pg_roles me ON me.oid=m.member WHERE m.roleid=r.oid AND me.rolname=current_user AND m.admin_option)
 ) ORDER BY wanted.name) FROM unnest($1::text[]) wanted(name) LEFT JOIN pg_roles r ON r.rolname=wanted.name),
 'schemas', (SELECT json_agg(json_build_object('name',wanted.name,'exists',n.oid IS NOT NULL,'owner',pg_get_userbyid(n.nspowner),
   'usage',CASE WHEN n.oid IS NULL THEN false ELSE has_schema_privilege(current_user,n.oid,'USAGE') END,
   'create',CASE WHEN n.oid IS NULL THEN false ELSE has_schema_privilege(current_user,n.oid,'CREATE') END
 ) ORDER BY wanted.name) FROM unnest(ARRAY['public','kaana_admission']) wanted(name) LEFT JOIN pg_namespace n ON n.nspname=wanted.name),
 'providerCostFunction', (SELECT json_build_object('exists',p.oid IS NOT NULL,'owner',pg_get_userbyid(p.proowner),
   'execute',CASE WHEN p.oid IS NULL THEN false ELSE has_function_privilege(current_user,p.oid,'EXECUTE') END)
   FROM (VALUES(to_regprocedure('public.kaana_record_provider_attempt_events(jsonb)'))) wanted(oid) LEFT JOIN pg_proc p ON p.oid=wanted.oid)
)`

func readAccessMetadata(ctx context.Context, conn *pgx.Conn, roles []string) (json.RawMessage, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var report json.RawMessage
	if err = tx.QueryRow(ctx, inspectAccessSQL, roles).Scan(&report); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return report, nil
}

func runInspectAccess(args []string, output io.Writer, getenv func(string) string) error {
	if len(args) != 0 {
		return errors.New("inspect-access accepts no arguments")
	}
	config, err := credentialstore.VerifiedPostgresConfig(getenv("DATABASE_URL"))
	if err != nil {
		return errors.New("inspect-access database configuration unavailable")
	}
	config.MaxConns = 1
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	config.ConnConfig.RuntimeParams["lock_timeout"] = "1000"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("inspect-access database unavailable")
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return errors.New("inspect-access database connection refused")
	}
	defer conn.Release()
	report, err := readAccessMetadata(ctx, conn.Conn(), []string{"kaana_admission_runtime", "kaana_admission_admin", "kaana_migrator"})
	if err != nil {
		return errors.New("inspect-access metadata query refused")
	}
	return json.NewEncoder(output).Encode(report)
}
