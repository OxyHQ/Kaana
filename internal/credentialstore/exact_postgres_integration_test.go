package credentialstore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

func TestExactCredentialReadPostgres(t *testing.T) {
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		provider, key string
		position      int
		active        bool
	}{{"exactfixture", "chosen", 1, true}, {"exactfixture", "other", 2, true}, {"exactfixture", "disabled", 3, false}, {"differentfixture", "chosen", 1, true}} {
		_, err := pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,key_class,enabled) VALUES($1,$2,'\x01',$3,$4,'paid',$5)`, row.provider, row.key, kmsTestKeyARN, row.position, row.active)
		if err != nil {
			t.Fatal(err)
		}
	}
	row, err := repository.GetEnabled(ctx, Scope{Provider: "exactfixture", KeyID: "chosen"})
	if err != nil || row.Provider != "exactfixture" || row.KeyID != "chosen" || row.Position != 1 {
		t.Fatalf("exact row: %+v %v", row, err)
	}
	for _, scope := range []Scope{{Provider: "exactfixture", KeyID: "missing"}, {Provider: "exactfixture", KeyID: "disabled"}, {Provider: "absentfixture", KeyID: "chosen"}} {
		if _, err := repository.GetEnabled(ctx, scope); !errors.Is(err, ErrCredentialNotActive) {
			t.Fatal("missing/disabled/provider mismatch did not fail closed", err)
		}
	}
}
