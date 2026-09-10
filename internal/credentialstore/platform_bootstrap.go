package credentialstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
)

const platformControlLogin = "kaana_platform_credential_control_login"

// BootstrapPlatformCredentialControl creates the two deliberately unprivileged
// database identities and applies every migration in one transaction. The
// control URL supplies only the login identity and password; all SQL runs over
// the already-open administrative connection.
func (p *Postgres) BootstrapPlatformCredentialControl(ctx context.Context, controlDatabaseURL string) error {
	password, err := platformControlPassword(controlDatabaseURL, p.databaseIdentity)
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("credential store: beginning platform control bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A transaction-local custom setting transports the bound parameter into
	// the utility statement. PostgreSQL therefore sees `$1` in the logged query,
	// never the password or a password verifier.
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('kaana.platform_control_password', $1, true)`, password); err != nil {
		return errors.New("credential store: staging platform control password failed")
	}
	if _, err := tx.Exec(ctx, `
DO $platform_control_bootstrap$
DECLARE
    role_record RECORD;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'kaana_platform_credential_control') THEN
        CREATE ROLE kaana_platform_credential_control NOLOGIN NOINHERIT
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'kaana_platform_credential_control_login') THEN
        CREATE ROLE kaana_platform_credential_control_login LOGIN INHERIT
            NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;

    FOR role_record IN
        SELECT rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb,
               rolcanlogin, rolreplication, rolbypassrls
        FROM pg_catalog.pg_roles
        WHERE rolname IN ('kaana_platform_credential_control', 'kaana_platform_credential_control_login')
    LOOP
        IF role_record.rolsuper OR role_record.rolcreaterole OR role_record.rolcreatedb
           OR role_record.rolreplication OR role_record.rolbypassrls
           OR (role_record.rolname = 'kaana_platform_credential_control' AND (role_record.rolcanlogin OR role_record.rolinherit))
           OR (role_record.rolname = 'kaana_platform_credential_control_login' AND (NOT role_record.rolcanlogin OR NOT role_record.rolinherit)) THEN
            RAISE EXCEPTION 'platform credential control role attributes are unsafe';
        END IF;
    END LOOP;

    IF EXISTS (
        SELECT 1
        FROM pg_catalog.pg_auth_members membership
        JOIN pg_catalog.pg_roles member_role ON member_role.oid = membership.member
        JOIN pg_catalog.pg_roles granted_role ON granted_role.oid = membership.roleid
        WHERE member_role.rolname IN ('kaana_platform_credential_control', 'kaana_platform_credential_control_login')
          AND NOT (
              member_role.rolname = 'kaana_platform_credential_control_login'
              AND granted_role.rolname = 'kaana_platform_credential_control'
          )
    ) THEN
        RAISE EXCEPTION 'platform credential control roles have unexpected memberships';
    END IF;

    GRANT kaana_platform_credential_control TO kaana_platform_credential_control_login;
    EXECUTE pg_catalog.format(
        'ALTER ROLE kaana_platform_credential_control_login PASSWORD %L',
        pg_catalog.current_setting('kaana.platform_control_password')
    );
    PERFORM pg_catalog.set_config('kaana.platform_control_password', '', true);
END
$platform_control_bootstrap$;`); err != nil {
		// PostgreSQL may attach the dynamically executed ALTER ROLE as an
		// InternalQuery. Never wrap that error: it could contain the verifier.
		return errors.New("credential store: creating platform control roles failed")
	}
	if err := migratePostgres(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("credential store: committing platform control bootstrap: %w", err)
	}
	return nil
}

func platformControlPassword(controlDatabaseURL string, expected postgresDatabaseIdentity) (string, error) {
	if controlDatabaseURL == "" {
		return "", errors.New("credential store: KAANA_PLATFORM_CREDENTIAL_CONTROL_DATABASE_URL is required")
	}
	config, err := pgxpool.ParseConfig(controlDatabaseURL)
	if err != nil {
		return "", errors.New("credential store: KAANA_PLATFORM_CREDENTIAL_CONTROL_DATABASE_URL is invalid")
	}
	if err := requireVerifiedPostgresTLS(config); err != nil {
		return "", errors.New("credential store: platform control DATABASE_URL must use sslmode=verify-full with a trusted root")
	}
	parsed, err := url.Parse(controlDatabaseURL)
	if err != nil {
		return "", errors.New("credential store: KAANA_PLATFORM_CREDENTIAL_CONTROL_DATABASE_URL is invalid")
	}
	query := parsed.Query()
	if modes, roots := query["sslmode"], query["sslrootcert"]; len(modes) != 1 || modes[0] != "verify-full" || len(roots) != 1 || roots[0] == "" {
		return "", errors.New("credential store: platform control DATABASE_URL must use exact sslmode=verify-full and one trusted root")
	}
	identity := postgresDatabaseIdentity{host: config.ConnConfig.Host, port: config.ConnConfig.Port, database: config.ConnConfig.Database}
	if identity != expected {
		return "", errors.New("credential store: platform control DATABASE_URL targets a different PostgreSQL database")
	}
	if config.ConnConfig.User != platformControlLogin {
		return "", fmt.Errorf("credential store: platform control DATABASE_URL user must be %s", platformControlLogin)
	}
	if config.ConnConfig.Password == "" {
		return "", errors.New("credential store: platform control DATABASE_URL password is required")
	}
	return config.ConnConfig.Password, nil
}
