package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

type fakePurger struct {
	remaining map[credentialstore.RetentionTable]int64
	calls     []string
	dryRuns   int
	cutoffs   map[credentialstore.RetentionTable]time.Time
}

func (f *fakePurger) PurgeRetention(_ context.Context, table credentialstore.RetentionTable, olderThan time.Time, limit int, dryRun bool) (int64, error) {
	f.calls = append(f.calls, string(table))
	if f.cutoffs == nil {
		f.cutoffs = map[credentialstore.RetentionTable]time.Time{}
	}
	f.cutoffs[table] = olderThan
	available := min(f.remaining[table], int64(limit))
	if dryRun {
		f.dryRuns++
		return available, nil
	}
	f.remaining[table] -= available
	return available, nil
}

func runPurge(t *testing.T, store *fakePurger, actor string, arguments ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := purgeRetentionCommand(context.Background(), arguments, func() (retentionPurger, func(), error) { return store, func() {}, nil },
		actor, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), &out)
	return out.String(), err
}

func TestPurgeRetentionIsADryRunUnlessAskedToApply(t *testing.T) {
	store := &fakePurger{remaining: map[credentialstore.RetentionTable]int64{credentialstore.RetentionCostEvents: 7, credentialstore.RetentionAttemptEvents: 3, credentialstore.RetentionSettlementsOut: 2}}
	out, err := runPurge(t, store, "")
	if err != nil {
		t.Fatalf("a dry run needs no actor: %v", err)
	}
	if store.dryRuns != 3 || store.remaining[credentialstore.RetentionCostEvents] != 7 {
		t.Errorf("a dry run deleted or skipped a table: %+v", store)
	}
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "7 eligible") {
		t.Errorf("output = %q", out)
	}
}

func TestPurgeRetentionApplyNeedsAnActorAndRunsBatchesUntilShort(t *testing.T) {
	store := &fakePurger{remaining: map[credentialstore.RetentionTable]int64{credentialstore.RetentionCostEvents: 25, credentialstore.RetentionAttemptEvents: 0, credentialstore.RetentionSettlementsOut: 10}}
	if _, err := runPurge(t, store, "", "--apply", "--batch", "10"); err == nil {
		t.Fatal("deleting without a named actor was allowed")
	}
	out, err := runPurge(t, store, "nate", "--apply", "--batch", "10")
	if err != nil {
		t.Fatal(err)
	}
	if store.remaining[credentialstore.RetentionCostEvents] != 0 || store.remaining[credentialstore.RetentionSettlementsOut] != 0 {
		t.Errorf("rows left after apply: %+v", store.remaining)
	}
	if !strings.Contains(out, "25 deleted") || !strings.Contains(out, "applied by nate") {
		t.Errorf("output = %q", out)
	}
}

func TestPurgeRetentionRefusesAnUnsafeWindowBeforeTouchingTheDatabase(t *testing.T) {
	for _, arguments := range [][]string{
		{"--cost-events-days", "29"}, {"--attempt-events-days", "1"}, {"--settlements-days", "0"},
		{"--batch", "0"}, {"--batch", "10001"}, {"stray"},
	} {
		store := &fakePurger{remaining: map[credentialstore.RetentionTable]int64{}}
		if _, err := runPurge(t, store, "nate", append([]string{"--apply"}, arguments...)...); err == nil {
			t.Errorf("%v was accepted", arguments)
		}
		if len(store.calls) != 0 {
			t.Errorf("%v reached the database", arguments)
		}
	}
}

func TestPurgeRetentionUsesTheDocumentedWindows(t *testing.T) {
	store := &fakePurger{remaining: map[credentialstore.RetentionTable]int64{}}
	if _, err := runPurge(t, store, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for table, days := range map[credentialstore.RetentionTable]int{
		credentialstore.RetentionCostEvents: 395, credentialstore.RetentionAttemptEvents: 395, credentialstore.RetentionSettlementsOut: 90,
	} {
		if got := now.Sub(store.cutoffs[table]); got != time.Duration(days)*24*time.Hour {
			t.Errorf("%s keeps %v, want %d days", table, got, days)
		}
	}
}
