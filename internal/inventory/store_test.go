package inventory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Pensara/internal/inventory"
)

func write(t *testing.T, path string, document []byte) {
	t.Helper()
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func store(t *testing.T, document []byte, now func() time.Time) (*inventory.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.json")
	write(t, path, document)
	built, err := inventory.NewStore(inventory.Config{Path: path, MaxSnapshotAge: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("building the store: %v", err)
	}
	return built, path
}

// TestAFailedReloadKeepsServingTheLastGoodSnapshot is the control-plane outage
// this type exists for. The publisher writing a truncated file, or writing
// nothing at all, must not take the data plane down with it.
func TestAFailedReloadKeepsServingTheLastGoodSnapshot(t *testing.T) {
	now := time.Now()
	built, path := store(t, issued(now, twoRevisions), func() time.Time { return now })

	// The control: a good reload does install, so the assertion below is about
	// the failure and not about Reload being inert.
	write(t, path, issued(now, twoDeploymentsOfOneRevision))
	if err := built.Reload(); err != nil {
		t.Fatalf("a valid snapshot failed to reload: %v", err)
	}
	if set, err := built.Current().Resolve("openai/gpt-5@2026-05-01", now); err != nil || set.Len() != 2 {
		t.Fatalf("the second snapshot was not installed: %v", err)
	}

	write(t, path, []byte(`{"snapshotId":"snap_broken","issuedAt":`))
	if err := built.Reload(); err == nil {
		t.Fatal("a truncated snapshot was installed")
	}

	set, err := built.Current().Resolve("openai/gpt-5@2026-05-01", now)
	if err != nil {
		t.Fatalf("after a failed reload the store stopped serving: %v", err)
	}
	if set.Len() != 2 {
		t.Errorf("after a failed reload the store is serving %d endpoints, not the last good snapshot's 2", set.Len())
	}
	if status := built.Status(); status.LastReloadError == "" {
		t.Error("the health projection does not report that the snapshot has stopped advancing")
	}
}

// TestAMissingSnapshotFileIsNotAStartup: a process with no snapshot at all has
// nothing to fall back TO, and serving nothing while reporting healthy is worse
// than refusing to boot.
func TestAMissingSnapshotFileIsNotAStartup(t *testing.T) {
	_, err := inventory.NewStore(inventory.Config{Path: filepath.Join(t.TempDir(), "absent.json")})
	if err == nil {
		t.Fatal("a store started with no snapshot")
	}
}

// TestStatusReportsWhenUnpinnedResolutionHasDegraded: an operator reading a
// wave of refusals needs to see the snapshot that stopped advancing, rather
// than inferring it from the shape of the errors.
func TestStatusReportsWhenUnpinnedResolutionHasDegraded(t *testing.T) {
	issuedAt := time.Now()
	clock := issuedAt
	built, _ := store(t, issued(issuedAt, twoRevisions), func() time.Time { return clock })

	fresh := built.Status()
	if !fresh.ServesUnpinnedReferences {
		t.Fatal("a snapshot issued now is already refusing unpinned references")
	}
	if fresh.SnapshotID != "snap_test" {
		t.Errorf("the projection names snapshot %q", fresh.SnapshotID)
	}
	if fresh.MaxAgeSeconds != int(time.Hour.Seconds()) {
		t.Errorf("the projection reports a horizon of %ds", fresh.MaxAgeSeconds)
	}

	clock = issuedAt.Add(2 * time.Hour)
	stale := built.Status()
	if stale.ServesUnpinnedReferences {
		t.Error("a two-hour-old snapshot under a one-hour horizon still reports unpinned references as served")
	}
	if stale.AgeSeconds < int(time.Hour.Seconds()) {
		t.Errorf("the projection reports an age of %ds", stale.AgeSeconds)
	}
}

// TestTheSnapshotProjectionCarriesNoPath: the health surface is signed and
// Oxy-facing, and a filesystem path is operational detail about the host rather
// than anything Oxy can act on.
func TestTheSnapshotProjectionCarriesNoPath(t *testing.T) {
	now := time.Now()
	built, path := store(t, issued(now, twoRevisions), func() time.Time { return now })

	// Both failure kinds, because they take different routes to the summary: a
	// parse error is produced here and names no path, while a read error comes
	// from the operating system and names one in its own message.
	write(t, path, []byte(`not json at all`))
	_ = built.Reload()
	assertNoPath(t, built.Status(), path)

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the snapshot: %v", err)
	}
	_ = built.Reload()
	assertNoPath(t, built.Status(), path)
}

func assertNoPath(t *testing.T, status inventory.SnapshotStatus, path string) {
	t.Helper()
	if status.LastReloadError == "" {
		t.Fatal("the reload failure was not recorded, so this check has nothing to inspect")
	}
	if strings.Contains(status.LastReloadError, path) || strings.Contains(status.LastReloadError, filepath.Dir(path)) {
		t.Errorf("the projection carries the snapshot's path: %q", status.LastReloadError)
	}
}
