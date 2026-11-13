package nativeadmission

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresMigrationAndImportUseDistinctOperatorRoles(t *testing.T) {
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid PostgreSQL test configuration")
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	name := newID("kaana_native_roles_")
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()+` OWNER kaana_migrator`); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	defer func() {
		_, err := admin.Exec(context.Background(), `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	}()
	connect := func(role string) *pgxpool.Pool {
		c := config.Copy()
		c.ConnConfig.Database = name
		c.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize())
			return err
		}
		p, err := pgxpool.NewWithConfig(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	migrating := connect("kaana_migrator")
	defer migrating.Close()
	if err = credentialstore.MigrateDatabase(ctx, migrating); err != nil {
		t.Fatal(err)
	}
	if err = NewStore(migrating).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	operator := connect("kaana_admission_admin")
	defer operator.Close()
	p := policy()
	store := NewStore(operator)
	if err = store.RegisterPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, review := testLegacyManifest(t, p)
	if _, err = store.ImportLegacyFence(ctx, raw, review); err != nil {
		t.Fatal(err)
	}
	runtime := connect("kaana_admission_runtime")
	defer runtime.Close()
	serving := NewStore(runtime)
	if _, err = serving.Admit(ctx, storeRequest(t, p, "req-role-proof", nil)); err != nil {
		t.Fatal(err)
	}
	if err = serving.RegisterPolicy(ctx, p); err == nil {
		t.Fatal("serving role registered policy")
	}
	if _, err = serving.ImportLegacyFence(ctx, raw, review); err == nil {
		t.Fatal("serving role activated enrollment")
	}
}
