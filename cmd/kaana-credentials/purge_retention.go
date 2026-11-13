package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

// Default retention, in days (docs/operating.md, "Data retention"). The
// database refuses a cutoff under credentialstore.MinimumRetention whatever is
// passed here.
const (
	defaultCostEventsDays    = 395 // 13 months: one financial year plus a month of close
	defaultAttemptEventsDays = 395
	defaultSettlementsDays   = 90 // delivered settlements only; Oxy reconciles within minutes
	defaultPurgeBatch        = 5000
	maxBatchesPerRun         = 100 // bounds one run to 100 batches per table
)

type retentionPurger interface {
	PurgeRetention(ctx context.Context, table credentialstore.RetentionTable, olderThan time.Time, limit int, dryRun bool) (int64, error)
}

// purgeRetentionCommand parses and runs `purge-retention`. It is a dry run
// unless --apply is given, so the only way to delete is to ask for it.
func purgeRetentionCommand(ctx context.Context, arguments []string, purger func() (retentionPurger, func(), error), actor string, now time.Time, stdout io.Writer) error {
	flags := flag.NewFlagSet("purge-retention", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	apply := flags.Bool("apply", false, "delete; without it nothing is deleted and the eligible counts are printed")
	costDays := flags.Int("cost-events-days", defaultCostEventsDays, "keep provider cost events this many days")
	attemptDays := flags.Int("attempt-events-days", defaultAttemptEventsDays, "keep credential attempt events this many days")
	settlementDays := flags.Int("settlements-days", defaultSettlementsDays, "keep DELIVERED settlements this many days")
	batch := flags.Int("batch", defaultPurgeBatch, "rows per batch")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("usage: kaana-credentials purge-retention [--apply] [--cost-events-days N] [--attempt-events-days N] [--settlements-days N] [--batch N]")
	}
	if *apply && actor == "" {
		return errors.New("KAANA_CREDENTIAL_ACTOR is required to delete")
	}
	minimumDays := int(credentialstore.MinimumRetention / (24 * time.Hour))
	for name, days := range map[string]int{"cost-events-days": *costDays, "attempt-events-days": *attemptDays, "settlements-days": *settlementDays} {
		if days < minimumDays {
			return fmt.Errorf("--%s must be at least %d: the database refuses a shorter retention", name, minimumDays)
		}
	}
	if *batch < 1 || *batch > credentialstore.MaxPurgeBatch {
		return fmt.Errorf("--batch must be 1 to %d", credentialstore.MaxPurgeBatch)
	}

	store, closeStore, err := purger()
	if err != nil {
		return err
	}
	defer closeStore()

	days := map[credentialstore.RetentionTable]int{
		credentialstore.RetentionCostEvents:     *costDays,
		credentialstore.RetentionAttemptEvents:  *attemptDays,
		credentialstore.RetentionSettlementsOut: *settlementDays,
	}
	mode := "dry run"
	if *apply {
		mode = "applied by " + actor
	}
	for _, table := range credentialstore.RetentionTables() {
		cutoff := now.Add(-time.Duration(days[table]) * 24 * time.Hour)
		var total int64
		for batches := 0; batches < maxBatchesPerRun; batches++ {
			affected, err := store.PurgeRetention(ctx, table, cutoff, *batch, !*apply)
			if err != nil {
				return err
			}
			total += affected
			// A dry run counts one batch's worth; a real run repeats until a
			// batch comes back short.
			if !*apply || affected < int64(*batch) {
				break
			}
		}
		verb := "eligible"
		if *apply {
			verb = "deleted"
		}
		if _, err := fmt.Fprintf(stdout, "%s (%s): %d %s older than %s (%d days)\n", table, mode, total, verb, cutoff.Format(time.RFC3339), days[table]); err != nil {
			return err
		}
	}
	return nil
}
