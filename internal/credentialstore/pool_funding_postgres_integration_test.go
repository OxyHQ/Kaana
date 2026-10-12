package credentialstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func freshHex(t *testing.T, prefix string) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(raw)
}

func runtimeRepositoryFor(t *testing.T, ctx context.Context, databaseURL string) *Postgres {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET SESSION AUTHORIZATION kaana_runtime`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Postgres{pool: pool}
}

// The pool is ordered by each key's stated funding, so the SERVING role must
// read the capacity category of a key's protected description — through the
// label-free economics function, never the description table itself.
func TestTheRuntimeReadsEachKeysCapacityCategoryForPoolOrder(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	providerSlug := "funding-" + freshHex(t, "")[:12]
	described, undescribed := "123e4567-e89b-42d3-a456-426614174011", "123e4567-e89b-42d3-a456-426614174012"
	for position, keyID := range []string{undescribed, described} {
		if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials
			(provider_slug, key_id, encrypted_secret, kms_key_arn, position)
			VALUES ($1, $2, '\x01', 'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', $3)`,
			providerSlug, keyID, position+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.PutCredentialMetadata(ctx, CredentialMetadata{
		SchemaVersion: CredentialMetadataSchemaVersion, OperationID: freshHex(t, "kcm_"),
		Provider: contract.ProviderSlug(providerSlug), KeyID: described, Title: "Monthly grant",
		FundingAccount:   FundingAccount{ID: freshHex(t, "kfa_"), Label: "operator@example.com"},
		CapacityCategory: CapacityPromotional, Environment: "production",
		CommercialUse: CommercialUseClaim{Eligibility: CommercialUseUnknown},
	}, "integration-test"); err != nil {
		t.Fatal(err)
	}

	rows, err := runtimeRepositoryFor(t, ctx, databaseURL).ListEnabled(ctx, []contract.ProviderSlug{contract.ProviderSlug(providerSlug)})
	if err != nil {
		t.Fatalf("the serving role cannot load its pool: %v", err)
	}
	categories := map[string]string{}
	for _, row := range rows {
		categories[row.KeyID] = row.CapacityCategory
	}
	if len(categories) != 2 || categories[described] != "promotional" || categories[undescribed] != "" {
		t.Fatalf("categories = %v", categories)
	}
	funding, err := provider.FundingFor(provider.KeyClassUnstated, categories[described])
	if err != nil || funding != provider.FundingPromotional {
		t.Fatalf("funding = %q, %v", funding, err)
	}
}

// An exact binding is an operator pin, and removing it is an audited,
// compare-and-delete mutation only credential administration may call.
func TestAnOperatorPinIsRemovedExactlyAndAudited(t *testing.T) {
	databaseURL := os.Getenv("KAANA_POSTGRES_TEST_URL")
	if databaseURL == "" {
		t.Skip("KAANA_POSTGRES_TEST_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &Postgres{pool: pool}
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	providerSlug := "unpin-" + freshHex(t, "")[:12]
	keyA, keyB := "123e4567-e89b-42d3-a456-426614174021", "123e4567-e89b-42d3-a456-426614174022"
	for position, keyID := range []string{keyA, keyB} {
		if _, err := pool.Exec(ctx, `INSERT INTO provider_credentials
			(provider_slug, key_id, encrypted_secret, kms_key_arn, position)
			VALUES ($1, $2, '\x01', 'arn:aws:kms:eu-west-1:123456789012:key/11111111-1111-1111-1111-111111111111', $3)`,
			providerSlug, keyID, position+1); err != nil {
			t.Fatal(err)
		}
	}
	pin := provider.CredentialBinding{DeploymentID: contract.DeploymentID("dep_" + providerSlug), Provider: contract.ProviderSlug(providerSlug), KeyID: keyA}
	bindOperation := freshHex(t, "kdb_")
	if outcome, err := repository.BindDeployment(ctx, bindOperation, pin, "operator:integration"); err != nil || outcome != "applied" {
		t.Fatalf("bind = %q, %v", outcome, err)
	}
	// An operation id already used for a bind is not an unpin.
	if _, err := repository.UnpinDeployment(ctx, bindOperation, pin, "operator:integration"); !errors.Is(err, ErrDeploymentBindingConflict) {
		t.Fatalf("a bind's operation id unpinned: %v", err)
	}
	// Naming another key than the current pin is a conflict, not a removal.
	wrong := pin
	wrong.KeyID = keyB
	if _, err := repository.UnpinDeployment(ctx, freshHex(t, "kdb_"), wrong, "operator:integration"); !errors.Is(err, ErrDeploymentBindingConflict) {
		t.Fatalf("an unpin naming the wrong key: %v", err)
	}
	unpin := freshHex(t, "kdb_")
	for _, want := range []string{"applied", "replayed"} {
		if outcome, err := repository.UnpinDeployment(ctx, unpin, pin, "operator:integration"); err != nil || outcome != want {
			t.Fatalf("unpin = %q, %v; want %q", outcome, err, want)
		}
	}
	if _, err := repository.UnpinDeployment(ctx, freshHex(t, "kdb_"), pin, "operator:integration"); !errors.Is(err, ErrDeploymentNotPinned) {
		t.Fatalf("unpinning a deployment with no pin: %v", err)
	}
	// An unpin's id replayed as a bind is a conflict.
	if _, err := repository.BindDeployment(ctx, unpin, pin, "operator:integration"); !errors.Is(err, ErrDeploymentBindingConflict) {
		t.Fatalf("an unpin's operation id was replayed as a bind: %v", err)
	}
	bindings, err := runtimeRepositoryFor(t, ctx, databaseURL).LoadDeploymentBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if binding.DeploymentID == pin.DeploymentID {
			t.Fatalf("the unpinned deployment still loads a pin: %+v", binding)
		}
	}
	var actions []string
	rows, err := pool.Query(ctx, `SELECT action FROM provider_deployment_binding_operations WHERE deployment_id = $1 ORDER BY completed_at, action`, pin.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	rows.Close()
	if len(actions) != 2 || actions[0] != "bind" || actions[1] != "unpin" {
		t.Fatalf("audit actions = %v", actions)
	}
	var runtimeMayUnpin, adminMayUnpin bool
	if err := pool.QueryRow(ctx, `SELECT
		has_function_privilege('kaana_runtime', 'kaana_unpin_provider_deployment(text,text,text,text,text)', 'EXECUTE'),
		has_function_privilege('kaana_credential_admin', 'kaana_unpin_provider_deployment(text,text,text,text,text)', 'EXECUTE')`).Scan(&runtimeMayUnpin, &adminMayUnpin); err != nil {
		t.Fatal(err)
	}
	if runtimeMayUnpin || !adminMayUnpin {
		t.Fatalf("unpin authority: runtime=%v admin=%v", runtimeMayUnpin, adminMayUnpin)
	}
}

// The reviewed 0021 path: inspection is read-only and reports 0021 pending;
// apply installs exactly the admin-only unpin mutation; a repeat is a no-op.
func TestReviewedUnpinMigrationAppliesOnlyMigration0021(t *testing.T) {
	repo, ctx := reviewedMigrationFixture(t)
	// The fixture stops before 0020: the 0021 path refuses that state.
	if _, err := repo.ReviewedUnpinMigration(ctx, true); err == nil {
		t.Fatal("0021 was admitted with 0020 still pending")
	}
	if _, err := repo.ReviewedScopedMigration(ctx, false); err != nil {
		t.Fatal(err)
	}
	inspected, err := repo.ReviewedUnpinMigration(ctx, true)
	if err != nil || len(inspected.Pending) != 1 || inspected.Pending[0] != "0021" || !inspected.ReadOnly || inspected.UnpinInstalled {
		t.Fatalf("inspection = %+v, %v", inspected, err)
	}
	applied, err := repo.ReviewedUnpinMigration(ctx, false)
	if err != nil || !applied.UnpinInstalled {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	again, err := repo.ReviewedUnpinMigration(ctx, true)
	if err != nil || len(again.Pending) != 0 || !again.UnpinInstalled {
		t.Fatalf("after apply = %+v, %v", again, err)
	}
	// The 0020 path still proves its own state with 0021 applied beside it.
	if report, err := repo.ReviewedScopedMigration(ctx, true); err != nil || len(report.Pending) != 0 {
		t.Fatalf("0020 inspection after 0021 = %+v, %v", report, err)
	}
}
