package credentialstore

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPlatformControlURLMustMatchExactDatabaseAndLogin(t *testing.T) {
	expected := postgresDatabaseIdentity{host: "db.example.invalid", port: 5432, database: "kaana"}
	valid := "postgres://kaana_platform_credential_control_login:control-secret@db.example.invalid:5432/kaana?sslmode=verify-full&sslrootcert=system"
	password, err := platformControlPassword(valid, expected)
	if err != nil {
		t.Fatalf("platformControlPassword: %v", err)
	}
	if password != "control-secret" {
		t.Fatal("platform control password was not parsed")
	}

	for name, databaseURL := range map[string]string{
		"absent":         "",
		"wrong login":    strings.Replace(valid, platformControlLogin, "kaana_runtime", 1),
		"wrong host":     strings.Replace(valid, "db.example.invalid", "other.example.invalid", 1),
		"wrong database": strings.Replace(valid, "/kaana?", "/other?", 1),
		"no password":    strings.Replace(valid, ":control-secret@", "@", 1),
		"unverified TLS": strings.Replace(valid, "verify-full", "require", 1),
		"plaintext":      strings.Replace(valid, "verify-full", "disable", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := platformControlPassword(databaseURL, expected); err == nil {
				t.Fatal("unsafe platform control URL was accepted")
			} else if strings.Contains(err.Error(), "control-secret") {
				t.Fatal("error disclosed the database secret")
			}
		})
	}
}

// masterIdentity is the identity OpenPostgres records for a master URL.
func masterIdentity(t *testing.T, databaseURL string) postgresDatabaseIdentity {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("ParseConfig master URL: %v", err)
	}
	identity, _ := postgresIdentity(&config.ConnConfig.Config)
	return identity
}

// PlanetScale routes a login `<role>.<branch id>` to that branch, and several
// branches share one host, port and database name. The control login is
// therefore the exact role plus the master connection's own branch id, and
// nothing else.
func TestPlatformControlURLAcceptsPlanetScaleLoginOnTheMasterBranch(t *testing.T) {
	const tail = "@us-east-3.pg.psdb.cloud:5432/postgres?sslmode=verify-full&sslrootcert=system"
	expected := masterIdentity(t, "postgres://postgres.cnlmx96ec5kw:master-secret"+tail)
	if expected.branch != "cnlmx96ec5kw" {
		t.Fatalf("master branch = %q, want the PlanetScale branch id", expected.branch)
	}
	password, err := platformControlPassword("postgres://"+platformControlLogin+".cnlmx96ec5kw:control-secret"+tail, expected)
	if err != nil {
		t.Fatalf("platformControlPassword PlanetScale login: %v", err)
	}
	if password != "control-secret" {
		t.Fatal("platform control password was not parsed")
	}

	for name, user := range map[string]string{
		"exact name without the branch":  platformControlLogin,
		"another branch":                 platformControlLogin + ".nk35mx55qq",
		"replica routing":                platformControlLogin + ".cnlmx96ec5kw|replica",
		"uppercase branch":               platformControlLogin + ".CNLMX96EC5KW",
		"short suffix":                   platformControlLogin + ".cnlmx96",
		"punctuated suffix":              platformControlLogin + ".cnlmx96-ec5kw",
		"empty suffix":                   platformControlLogin + ".",
		"two suffixes":                   platformControlLogin + ".cnlmx96ec5kw.cnlmx96ec5kw",
		"prefixed role":                  "admin." + platformControlLogin + ".cnlmx96ec5kw",
		"extended role":                  platformControlLogin + "_x.cnlmx96ec5kw",
		"truncated role":                 strings.TrimSuffix(platformControlLogin, "_login") + ".cnlmx96ec5kw",
		"authorization role":             "kaana_platform_credential_control.cnlmx96ec5kw",
		"master role on the same branch": "postgres.cnlmx96ec5kw",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := platformControlPassword("postgres://"+user+":control-secret"+tail, expected); err == nil {
				t.Fatalf("platform control login %q was accepted", user)
			} else if strings.Contains(err.Error(), "control-secret") {
				t.Fatal("error disclosed the database secret")
			}
		})
	}
}

// A server without branch routing keeps the exact login: a suffix that only
// looks like a PlanetScale branch id is not stripped from a non-PlanetScale
// master, so it cannot widen the accepted name there.
func TestPlatformControlURLRejectsBranchSuffixWhenMasterHasNone(t *testing.T) {
	const tail = "@db.example.invalid:5432/kaana?sslmode=verify-full&sslrootcert=system"
	expected := masterIdentity(t, "postgres://kaana_admin:master-secret"+tail)
	if expected.branch != "" {
		t.Fatalf("non-PlanetScale master gained branch %q", expected.branch)
	}
	if _, err := platformControlPassword("postgres://"+platformControlLogin+":control-secret"+tail, expected); err != nil {
		t.Fatalf("exact login refused: %v", err)
	}
	if _, err := platformControlPassword("postgres://"+platformControlLogin+".cnlmx96ec5kw:control-secret"+tail, expected); err == nil {
		t.Fatal("branch-suffixed login was accepted against a master without one")
	}
}

func TestSplitPostgresLoginStripsOnlyABranchID(t *testing.T) {
	for user, want := range map[string][2]string{
		"kaana_runtime":                      {"kaana_runtime", ""},
		"kaana_runtime.cnlmx96ec5kw":         {"kaana_runtime", "cnlmx96ec5kw"},
		"kaana_runtime.nk35mx55qq":           {"kaana_runtime", "nk35mx55qq"},
		"a.b.cnlmx96ec5kw":                   {"a.b", "cnlmx96ec5kw"},
		"kaana_runtime.cnlmx96ec5kw|replica": {"kaana_runtime.cnlmx96ec5kw|replica", ""},
		"kaana_runtime.Cnlmx96ec5kw":         {"kaana_runtime.Cnlmx96ec5kw", ""},
		"kaana_runtime.":                     {"kaana_runtime.", ""},
	} {
		role, branch := splitPostgresLogin(user)
		if role != want[0] || branch != want[1] {
			t.Errorf("splitPostgresLogin(%q) = (%q, %q), want (%q, %q)", user, role, branch, want[0], want[1])
		}
	}
}

func TestPlatformRoleCreationSQLNeverEmbedsASecretOrRunsMigrations(t *testing.T) {
	source := readSourceFile(t, "platform_bootstrap.go")
	for _, required := range []string{
		"pg_catalog.set_config('kaana.platform_control_password', $1, true)",
		"CREATE ROLE kaana_platform_credential_control NOLOGIN NOINHERIT",
		"CREATE ROLE kaana_platform_credential_control_login LOGIN INHERIT",
		"GRANT kaana_platform_credential_control TO kaana_platform_credential_control_login",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("bootstrap lost %q", required)
		}
	}
	if strings.Contains(source, "migratePostgres(ctx, tx)") {
		t.Fatal("master-only role creation must not run schema migrations")
	}
	for _, forbidden := range []string{"os.Args", "slog.", "fmt.Println", "PASSWORD $1"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("bootstrap gained unsafe secret transport %q", forbidden)
		}
	}
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}
