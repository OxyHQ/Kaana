package commercialadmission

import (
	"context"
	"errors"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Runtime connects the fixed commercial hooks and their durable recovery to
// the same pool. Construction never migrates, enrolls, funds or settles anything.
type Runtime struct {
	Store       *nativeadmission.Store
	coordinator *Coordinator
	pool        *pgxpool.Pool
}

func (r *Runtime) Close() { r.pool.Close() }

func (r *Runtime) Recover(ctx context.Context, limit int) error {
	_, err := r.coordinator.Recover(ctx, r.Store, limit)
	return err
}

// OpenRuntime requires a separately provisioned NOLOGIN role that inherits only
// admission and billing runtime permissions. In particular it need not inherit
// kaana_runtime: admission_runtime already has the exact cost-record function.
func OpenRuntime(ctx context.Context, databaseURL string) (*Runtime, error) {
	config, err := runtimeConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	return openConfiguredRuntime(ctx, config)
}

func runtimeConfig(databaseURL string) (*pgxpool.Config, error) {
	config, err := credentialstore.VerifiedPostgresConfig(databaseURL)
	if err != nil {
		return nil, errors.New("commercialadmission: invalid database configuration")
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE kaana_commercial_runtime`)
		return err
	}
	return config, nil
}

func openConfiguredRuntime(ctx context.Context, config *pgxpool.Config) (*Runtime, error) {
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("commercialadmission: runtime database unavailable")
	}
	if err = verifyRuntime(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	coordinator := New(pool)
	return &Runtime{Store: nativeadmission.NewStoreWithEconomics(pool, coordinator), coordinator: coordinator, pool: pool}, nil
}

// Check actual effective privileges, including the login's ability to SET an
// administrative role. A misprovisioned serving identity must fail at startup,
// not silently acquire funding or policy-management rights.
func verifyRuntime(ctx context.Context, pool *pgxpool.Pool) error {
	var valid bool
	err := pool.QueryRow(ctx, `
SELECT current_user='kaana_commercial_runtime'
 AND NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN (current_user,session_user)
                 AND (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls))
 AND NOT (SELECT rolcanlogin FROM pg_roles WHERE rolname=current_user)
 AND pg_has_role(current_user,'kaana_admission_runtime','USAGE')
 AND pg_has_role(current_user,'kaana_billing_runtime','USAGE')
 AND NOT pg_has_role(current_user,'kaana_runtime','MEMBER')
 AND NOT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.roleid
                 WHERE m.member=current_user::regrole AND r.rolname NOT IN ('kaana_admission_runtime','kaana_billing_runtime'))
 AND NOT EXISTS (SELECT 1 FROM pg_auth_members
                 WHERE member IN (current_user::regrole,session_user::regrole) AND admin_option)
 AND NOT EXISTS (
   SELECT 1 FROM pg_roles WHERE rolname IN ('kaana_migrator','kaana_admission_admin','kaana_billing_admin','kaana_billing_migrator')
   AND (pg_has_role(current_user,oid,'MEMBER') OR pg_has_role(session_user,oid,'MEMBER')))
 AND has_table_privilege(current_user,'kaana_admission.admissions','INSERT')
 AND has_table_privilege(current_user,'kaana_admission.completions','INSERT')
 AND has_table_privilege(current_user,'kaana_admission.settlements','INSERT')
 AND has_table_privilege(current_user,'kaana_commercial.admissions','INSERT')
 AND has_table_privilege(current_user,'kaana_commercial.outcomes','INSERT')
 AND has_table_privilege(current_user,'kaana_billing.accounts','SELECT')
 AND has_table_privilege(current_user,'kaana_billing.holds','SELECT')
 AND NOT has_table_privilege(current_user,'kaana_admission.policy_versions','INSERT')
 AND NOT has_table_privilege(current_user,'kaana_admission.enrollments','UPDATE')
 AND NOT has_table_privilege(current_user,'kaana_billing.accounts','UPDATE')
 AND NOT has_table_privilege(current_user,'kaana_billing.journal','INSERT')
 AND NOT has_schema_privilege(current_user,'kaana_admission','CREATE')
 AND NOT has_schema_privilege(current_user,'kaana_billing','CREATE')
 AND NOT has_schema_privilege(current_user,'kaana_commercial','CREATE')
 AND has_function_privilege(current_user,'public.kaana_record_provider_attempt_events(jsonb)','EXECUTE')
 AND has_function_privilege(current_user,'kaana_billing.reserve(text,text,text,text,text,text,integer)','EXECUTE')
 AND has_function_privilege(current_user,'kaana_billing.settle(text,text,text,text,text,text,boolean,text)','EXECUTE')
 AND has_function_privilege(current_user,'kaana_billing.expire(text)','EXECUTE')
 AND NOT has_function_privilege(current_user,'kaana_billing.create_account(text,text)','EXECUTE')
 AND NOT has_function_privilege(current_user,'kaana_billing.configure_account(text,text,text,boolean)','EXECUTE')
 AND NOT has_function_privilege(current_user,'kaana_billing.configure_budget(text,text,timestamptz,timestamptz,text)','EXECUTE')
 AND NOT has_function_privilege(current_user,'kaana_billing.fund(text,text,text,text,text)','EXECUTE')
 AND NOT has_function_privilege(current_user,'kaana_billing.refund(text,text,text,text,text)','EXECUTE')`).Scan(&valid)
	if err != nil || !valid {
		return errors.New("commercialadmission: runtime schema or least-privilege role is not ready")
	}
	return nil
}
