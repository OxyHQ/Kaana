package credentialstore

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/sessionowner"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRealtimeOwnerDirectoryIsEphemeralCASWithRuntimeFunctionsOnly(t *testing.T) {
	_, admin, ctx := openSettlementTestStore(t)
	config := admin.Config()
	config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE kaana_runtime")
		return err
	}
	runtime, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	directory := RealtimeOwners{Store: &Postgres{pool: runtime}}
	entry := sessionowner.Entry{RequestID: "peer_cas_" + time.Now().Format("150405.000000000"), Lane: sessionowner.Public, Address: "10.2.3.4", Generation: strings.Repeat("a", 32)}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DELETE FROM realtime_session_owners WHERE request_id=$1", entry.RequestID)
	})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, claimErr := directory.Claim(ctx, entry)
			if claimErr != nil {
				t.Error(claimErr)
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("concurrent claims won %d times", winners.Load())
	}
	got, found, err := directory.Lookup(ctx, entry.RequestID, entry.Lane)
	if err != nil || !found || got.Address != entry.Address || got.Generation != entry.Generation || time.Until(got.ExpiresAt) > sessionowner.LeaseDuration || time.Until(got.ExpiresAt) < 40*time.Second {
		t.Fatalf("invalid lease: %+v %v %v", got, found, err)
	}
	if _, found, err = directory.Lookup(ctx, entry.RequestID, sessionowner.Internal); err != nil || found {
		t.Fatal("lane leaked", err)
	}
	other := entry
	other.Generation = strings.Repeat("b", 32)
	if ok, err := directory.Renew(ctx, other); err != nil || ok {
		t.Fatal("stale process renewed", err)
	}
	if err = directory.Release(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, found, _ = directory.Lookup(ctx, entry.RequestID, entry.Lane); !found {
		t.Fatal("stale process removed owner")
	}
	if ok, err := directory.Renew(ctx, entry); err != nil || !ok {
		t.Fatal("live owner did not renew", err)
	}
	if _, err = runtime.Exec(ctx, "SELECT * FROM realtime_session_owners"); err == nil {
		t.Fatal("runtime can read directory without function")
	}
	if _, err = runtime.Exec(ctx, "DELETE FROM realtime_session_owners"); err == nil {
		t.Fatal("runtime has direct DML")
	}
	if _, err = admin.Exec(ctx, "UPDATE realtime_session_owners SET expires_at=clock_timestamp()-interval '1 second' WHERE request_id=$1", entry.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ = directory.Lookup(ctx, entry.RequestID, entry.Lane); found {
		t.Fatal("expired owner exposed")
	}
	if ok, err := directory.Renew(ctx, entry); err != nil || ok {
		t.Fatal("expired owner resurrected", err)
	}
	if ok, err := directory.Claim(ctx, other); err != nil || !ok {
		t.Fatal("expired lease not reclaimed", err)
	}
	if err = directory.Release(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if current, found, _ := directory.Lookup(ctx, entry.RequestID, entry.Lane); !found || current.Generation != other.Generation {
		t.Fatal("late old release removed new generation")
	}
	if err = directory.Release(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, found, _ = directory.Lookup(ctx, entry.RequestID, entry.Lane); found {
		t.Fatal("ended owner retained")
	}
}
