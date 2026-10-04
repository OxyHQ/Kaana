package kaana

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This wrapper observes the real canonical SQL call, never supplies its result.
type observedSQLScopedClaims struct {
	repository  *credentialstore.Postgres
	observation *scopedClaimFixture
	last        credentialstore.ScopedAttemptClaim
}

func (c *observedSQLScopedClaims) ClaimScopedAttempt(ctx context.Context, claim credentialstore.ScopedAttemptClaim) (bool, error) {
	c.observation.mu.Lock()
	c.observation.calls++
	c.observation.mu.Unlock()
	c.last = claim
	return c.repository.ClaimScopedAttempt(ctx, claim)
}

func TestScopedExecutorSourceWindowUsesBoundedSQLClaimAndPermanentReplayDenial(t *testing.T) {
	databaseURL := os.Getenv("KAANA_SCOPED_WINDOW_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_SCOPED_WINDOW_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := credentialstore.OpenPostgres(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err = admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx, `INSERT INTO provider_credentials(provider_slug,key_id,encrypted_secret,kms_key_arn,position,key_class,enabled)
 VALUES('openrouter','exact-key','\x01','arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000001',1,'paid',true)`)
	if err != nil {
		t.Fatal(err)
	}
	var bound string
	err = pool.QueryRow(ctx, `SELECT kaana_bind_provider_deployment('kdb_000000000000000000000000000000cd','dep-private','openrouter','exact-key','synthetic-test')`).Scan(&bound)
	if err != nil || bound != "applied" {
		t.Fatal("binding failed", err, bound)
	}
	runtimeURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	options := runtimeURL.Query()
	options.Set("options", "-c role=kaana_runtime")
	runtimeURL.RawQuery = options.Encode()
	runtime, err := credentialstore.OpenPostgres(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, tc := range []struct {
		name     string
		lifetime time.Duration
		expired  bool
	}{
		{"six-hour source", 6 * time.Hour, false}, {"three-minute source", 3 * time.Minute, false}, {"expired source", 6 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Now().UTC().Truncate(time.Second)
			e, r, adapter, observations := scopedExecutorFixtureAt(t, at, tc.lifetime)
			claims := &observedSQLScopedClaims{repository: runtime, observation: observations}
			e.scopedClaims = claims
			if tc.expired {
				e.now = func() time.Time { return at.Add(tc.lifetime + time.Second) }
			}
			result := e.Execute(ctx, r, func(contract.StreamEvent) error { return nil })
			if tc.expired {
				if result.Failure == nil || adapter.sends != 0 || observations.calls != 0 {
					t.Fatalf("expired approval reached SQL/send: %+v sends=%d calls=%d", result.Failure, adapter.sends, observations.calls)
				}
				var count int
				if err = pool.QueryRow(ctx, `SELECT count(*) FROM scoped_provider_attempt_claims WHERE operation_id=$1`, r.ScopedExecution.PermitID).Scan(&count); err != nil || count != 0 {
					t.Fatal("expired claim persisted", err, count)
				}
				return
			}
			if result.Failure != nil || adapter.sends != 1 || observations.calls != 1 {
				t.Fatalf("real SQL admission failed: %+v sends=%d calls=%d", result.Failure, adapter.sends, observations.calls)
			}
			want := at.Add(tc.lifetime)
			if want.After(at.Add(5 * time.Minute)) {
				want = at.Add(5 * time.Minute)
			}
			var claimed, expires time.Time
			if err = pool.QueryRow(ctx, `SELECT claimed_at,expires_at FROM scoped_provider_attempt_claims WHERE operation_id=$1`, r.ScopedExecution.PermitID).Scan(&claimed, &expires); err != nil || !claimed.Equal(at) || !expires.Equal(want) || !claims.last.ExpiresAt.Equal(want) {
				t.Fatal("claim lifetime mismatch", err, claimed, expires, want)
			}
			if r.ScopedExecution.ExpiresAt != at.Add(tc.lifetime).Format(time.RFC3339) {
				t.Fatal("source expiry was changed")
			}
			// Only the owned fixture's timestamp is shortened; no wall-clock sleep or
			// production mutation. The PK must survive a genuinely SQL-expired lease.
			if _, err = pool.Exec(ctx, `UPDATE scoped_provider_attempt_claims SET expires_at=claimed_at+interval '1 millisecond' WHERE operation_id=$1`, r.ScopedExecution.PermitID); err != nil {
				t.Fatal(err)
			}
			var sqlExpired bool
			if err = pool.QueryRow(ctx, `SELECT expires_at < clock_timestamp() FROM scoped_provider_attempt_claims WHERE operation_id=$1`, r.ScopedExecution.PermitID).Scan(&sqlExpired); err != nil || !sqlExpired {
				t.Fatal("owned lease is not expired", err)
			}
			r.Attribution.RequestID = "replay-request"
			r.ScopedExecution.RequestID = "replay-request"
			replay := e.Execute(ctx, r, func(contract.StreamEvent) error { return nil })
			if replay.Failure == nil || adapter.sends != 1 || observations.calls != 2 {
				t.Fatal("expired lease reopened stable permit", replay.Failure, adapter.sends, observations.calls)
			}
			var count int
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM scoped_provider_attempt_claims WHERE operation_id=$1`, r.ScopedExecution.PermitID).Scan(&count); err != nil || count != 1 {
				t.Fatal("stable operation row changed", err, count)
			}
		})
	}
}
