package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/peablebilling"
	"github.com/OxyHQ/Kaana/internal/workloadidentity"
)

type peableOperatorStore interface {
	peablebilling.DraftStore
	peablebilling.CloseStore
	MigrateClose(context.Context) error
	Migrate(context.Context) error
	BindCustomer(context.Context, string, string, string) error
	PrepareForMerchant(context.Context, string, string, string, []string, int) (peablebilling.Draft, error)
}
type peableOperatorOpen func(context.Context, string, peablebilling.Operation) (peableOperatorStore, func(), error)
type peableClientOpen func(context.Context, peablebilling.OperatorConfig) (*peablebilling.Client, error)

func openPeableOperator(ctx context.Context, databaseURL string, op peablebilling.Operation) (peableOperatorStore, func(), error) {
	return peablebilling.OpenOperator(ctx, databaseURL, op)
}
func openPeableClient(ctx context.Context, cfg peablebilling.OperatorConfig) (*peablebilling.Client, error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	identity, err := workloadidentity.Open(ctx, workloadidentity.Config{Logger: logger})
	if err != nil {
		return nil, peablebilling.ErrMismatch
	}
	origin, _ := url.Parse("https://api.oxy.so")
	return peablebilling.NewClient(cfg.ClientConfig(), nil, func(ctx context.Context, audience string) (string, error) {
		token, _, err := identity.Mint(ctx, origin, contract.OxyAudience(audience))
		return token, err
	})
}

// Explicit offline command, never part of serving startup or deployment. The
// reviewed configuration and exact operation are checked before opening DB/AWS.
func runPeableBilling(args []string, input io.Reader, output io.Writer, getenv func(string) string, open peableOperatorOpen, clientOpen peableClientOpen) error {
	flags := flag.NewFlagSet("peable-billing", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "reviewed opt-in configuration")
	hash := flags.String("config-sha256", "", "reviewed exact configuration SHA256")
	operationPath := flags.String("operation-file", "", "private bounded operation document; otherwise stdin")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid Peable billing operator arguments")
	}
	cfg, err := peablebilling.ReadOperatorConfig(*path, *hash)
	if err != nil {
		return errors.New("peable billing operator configuration refused")
	}
	raw, err := readManifestArtifact(*operationPath, input)
	if err != nil || len(raw) > 262144 {
		return errors.New("peable billing operation document refused")
	}
	op, err := peablebilling.ReadOperation(strings.NewReader(string(raw)))
	if err != nil {
		return errors.New("peable billing operation refused")
	}
	if (op.Action == "migrate-close" || op.Action == "review-close" || op.Action == "close") && !cfg.FinalizationEnabled {
		return errors.New("peable invoice finalization is disabled")
	}
	databaseURL := strings.TrimSpace(getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("peable billing database configuration required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, closePool, err := open(ctx, databaseURL, op)
	if err != nil {
		return errors.New("peable billing role or schema unavailable")
	}
	defer closePool()
	var draft peablebilling.Draft
	var closeAttempt peablebilling.CloseAttempt
	var reviewRevision string
	switch op.Action {
	case "migrate":
		err = store.Migrate(ctx)
	case "migrate-close":
		err = store.MigrateClose(ctx)
	case "bind":
		err = store.BindCustomer(ctx, op.AccountID, cfg.MerchantID, op.CustomerID)
	case "prepare":
		draft, err = store.PrepareForMerchant(ctx, cfg.MerchantID, op.AccountID, op.Key, op.ReceiptIDs, op.DaysUntilDue)
	case "inspect":
		draft, err = store.Read(ctx, op.DraftID)
	case "review-close", "close":
		var client *peablebilling.Client
		client, err = clientOpen(ctx, cfg)
		if err == nil {
			var worker *peablebilling.CloseWorker
			worker, err = peablebilling.NewCloseWorker(cfg.FinalizationEnabled, store, client)
			if err == nil {
				if op.Action == "review-close" {
					reviewRevision, err = worker.Review(ctx, op.DraftID)
				} else {
					closeAttempt, err = worker.Close(ctx, op.DraftID, op.Key, op.Revision)
				}
			}
		}
	case "deliver", "reconcile-event":
		var client *peablebilling.Client
		client, err = clientOpen(ctx, cfg)
		if err == nil {
			var worker *peablebilling.Worker
			worker, err = peablebilling.NewWorker(store, client)
			if err == nil {
				if op.Action == "deliver" {
					draft, err = worker.Deliver(ctx, op.DraftID)
				} else {
					draft, err = worker.ReconcileEvent(ctx, op.EventID)
				}
			}
		}
	}
	if err != nil {
		return errors.New("peable billing operation pending or refused; reconcile the same operation before retrying")
	}
	if draft.ID != "" && draft.MerchantID != cfg.MerchantID {
		return errors.New("peable billing seller mismatch")
	}
	// No upstream error bodies, credentials, customer profile or invoice contents.
	if op.Action == "review-close" || op.Action == "close" {
		return json.NewEncoder(output).Encode(struct {
			Operation string `json:"operation"`
			Complete  bool   `json:"complete"`
			DraftID   string `json:"draftId"`
			Revision  string `json:"revision,omitempty"`
			CloseID   string `json:"closeId,omitempty"`
			State     string `json:"state,omitempty"`
		}{op.Action, true, op.DraftID, reviewRevision, closeAttempt.ID, closeAttempt.State})
	}
	result := struct {
		Operation string `json:"operation"`
		Complete  bool   `json:"complete"`
		DraftID   string `json:"draftId,omitempty"`
		InvoiceID string `json:"invoiceId,omitempty"`
		ItemID    string `json:"itemId,omitempty"`
	}{op.Action, true, draft.ID, draft.InvoiceID, draft.ItemID}
	return json.NewEncoder(output).Encode(result)
}
