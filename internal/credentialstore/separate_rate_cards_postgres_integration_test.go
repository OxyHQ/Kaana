package credentialstore

import (
	"context"
	"github.com/OxyHQ/Kaana/internal/providercost"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndependentCardObservationsRegisterAndReplayWithoutChangingXai(t *testing.T) {
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
	repo := &Postgres{pool: pool}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	jev := `{"schemaVersion":1,"rateCardVersionId":"rc_jev_registration_fixture","source":"provider_api","sourceVersion":"jev-api-oct4","observedAt":"2026-10-04T13:12:32Z","effectiveAt":"2026-10-04T13:12:32Z","expiresAt":"2026-10-04T13:17:32Z","rateCards":[{"deploymentId":"dep_jev_fixture","currency":"USD","rates":[{"unit":"input_tokens","amountPerUnit":42000},{"unit":"output_tokens","amountPerUnit":0}]}]}`
	file := filepath.Join(t.TempDir(), "jev.json")
	if err := os.WriteFile(file, []byte(jev), 0600); err != nil {
		t.Fatal(err)
	}
	cards, err := providercost.Load("../../configs/provider-rates.json", file)
	if err != nil {
		t.Fatal(err)
	}
	observations := cards.Observations()
	if len(observations) != 2 {
		t.Fatal("not two original documents")
	}
	for range 2 {
		for _, observation := range observations {
			if err := repo.RegisterRateCardVersion(ctx, observation); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, observation := range observations {
		var source, sourceVersion string
		var observed, effective time.Time
		var expires *time.Time
		err := pool.QueryRow(ctx, `SELECT source,source_version,observed_at,effective_at,expires_at FROM provider_rate_card_versions WHERE version_id=$1`, observation.VersionID).Scan(&source, &sourceVersion, &observed, &effective, &expires)
		if err != nil || source != string(observation.Source) || sourceVersion != observation.SourceVersion || !observed.Equal(observation.ObservedAt) || !effective.Equal(observation.EffectiveAt) || (expires == nil) != (observation.ExpiresAt == nil) || (expires != nil && !expires.Equal(*observation.ExpiresAt)) {
			t.Fatal("registered evidence changed", err)
		}
		changed := observation
		changed.ObservedAt = changed.ObservedAt.Add(time.Second)
		if err := repo.RegisterRateCardVersion(ctx, changed); err == nil {
			t.Fatal("existing observation could be re-dated")
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_rate_card_versions WHERE version_id IN ('rc_jev_registration_fixture','rc_xai_realtime_2026_09_30')`).Scan(&count); err != nil || count != 2 {
		t.Fatal("not exact two registrations", err)
	}
}
