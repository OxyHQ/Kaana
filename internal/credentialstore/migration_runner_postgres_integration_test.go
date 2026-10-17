package credentialstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A migration that cannot take its lock gives up after lock_timeout instead of
// queueing behind the holder, and is retried until the holder lets go.
func TestMigrateRetriesALockItCannotTake(t *testing.T) {
	url := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if url == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE kaana_schema_migrations IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = holder.Rollback(ctx)
		t.Fatal(err)
	}
	// The holder is used only by this goroutine from here on: a pgx.Tx is not
	// safe for concurrent use.
	held := migrationLockTimeout + 500*time.Millisecond
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(held)
		_ = holder.Rollback(context.Background())
	}()
	defer func() { <-released }()

	started := time.Now()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatalf("Migrate did not retry past the held lock: %v", err)
	}
	if waited := time.Since(started); waited < held {
		t.Fatalf("Migrate finished in %s, before the lock was released at %s", waited, held)
	}
}
