package credentialstore

import (
	"os"
	"strings"
	"testing"
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

func TestPlatformBootstrapSQLNeverEmbedsASecretValue(t *testing.T) {
	source := readSourceFile(t, "platform_bootstrap.go")
	for _, required := range []string{
		"pg_catalog.set_config('kaana.platform_control_password', $1, true)",
		"CREATE ROLE kaana_platform_credential_control NOLOGIN NOINHERIT",
		"CREATE ROLE kaana_platform_credential_control_login LOGIN INHERIT",
		"GRANT kaana_platform_credential_control TO kaana_platform_credential_control_login",
		"migratePostgres(ctx, tx)",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("bootstrap lost %q", required)
		}
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
