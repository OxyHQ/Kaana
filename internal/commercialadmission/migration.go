package commercialadmission

import (
	"context"
	"errors"

	"github.com/OxyHQ/Kaana/internal/billing"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/nativeadmission"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrateDatabase is an explicit offline operation. It prepares only schemas;
// no currency, account, policy, enrollment, payment or funding is created.
func MigrateDatabase(ctx context.Context, databaseURL string) error {
	config, err := migrationConfig(databaseURL)
	if err != nil {
		return err
	}
	return migrateConfigured(ctx, config)
}

func migrationConfig(databaseURL string) (*pgxpool.Config, error) {
	config, err := credentialstore.VerifiedPostgresConfig(databaseURL)
	if err != nil {
		return nil, errors.New("commercialadmission: invalid migration database configuration")
	}
	config.MaxConns = 1
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE kaana_billing_migrator`)
		return err
	}
	return config, nil
}

func migrateConfigured(ctx context.Context, config *pgxpool.Config) error {
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("commercialadmission: migration database unavailable")
	}
	defer pool.Close()
	var allowed bool
	err = pool.QueryRow(ctx, `SELECT current_user='kaana_billing_migrator'
 AND NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname IN (current_user,session_user)
                AND (rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls))
 AND NOT (SELECT rolcanlogin FROM pg_roles WHERE rolname=current_user)
 AND has_database_privilege(current_user,current_database(),'CREATE')
 AND has_schema_privilege(current_user,'public','CREATE')
 AND has_schema_privilege(current_user,'kaana_admission','USAGE')
 AND has_table_privilege(current_user,'kaana_admission.admissions','REFERENCES')
 AND NOT has_table_privilege(current_user,'kaana_admission.enrollments','UPDATE')
 AND NOT has_table_privilege(current_user,'kaana_admission.policy_versions','INSERT')`).Scan(&allowed)
	if err != nil || !allowed {
		return errors.New("commercialadmission: migration authority or native schema is not ready")
	}
	if err = nativeadmission.NewStore(pool).VerifyMigrations(ctx); err != nil {
		return err
	}
	if err = billing.NewStore(pool).Migrate(ctx); err != nil {
		return err
	}
	return New(pool).Migrate(ctx)
}
