package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/OxyHQ/Kaana/internal/billingaccounts"
)

type accountOperator interface {
	Migrate(context.Context) error
	Apply(context.Context, billingaccounts.Config, billingaccounts.Operation) (json.RawMessage, error)
}
type accountOpen func(context.Context, string, billingaccounts.Operation) (accountOperator, func(), error)

func openAccountOperator(ctx context.Context, url string, op billingaccounts.Operation) (accountOperator, func(), error) {
	return billingaccounts.OpenOperator(ctx, url, op)
}
func runBillingAccounts(args []string, input io.Reader, out io.Writer, getenv func(string) string, open accountOpen) error {
	flags := flag.NewFlagSet("billing-accounts", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "reviewed account scope")
	digest := flags.String("config-sha256", "", "reviewed exact config hash")
	operation := flags.String("operation-file", "", "private operation document; otherwise stdin")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid account operator arguments")
	}
	c, err := billingaccounts.ReadConfig(*path, *digest)
	if err != nil {
		return errors.New("account operator configuration refused")
	}
	raw, err := readManifestArtifact(*operation, input)
	if err != nil || len(raw) > 65536 {
		return errors.New("account operation document refused")
	}
	op, err := billingaccounts.ReadOperation(bytes.NewReader(raw))
	if err != nil {
		return errors.New("account operation refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, closePool, err := open(ctx, getenv("DATABASE_URL"), op)
	if err != nil {
		return errors.New("account operator authority unavailable")
	}
	defer closePool()
	var result json.RawMessage
	if op.Action == "migrate" {
		err = store.Migrate(ctx)
		result = json.RawMessage(`{"migration":"verified"}`)
	} else {
		result, err = store.Apply(ctx, c, op)
	}
	if err != nil {
		return errors.New("account operation did not complete; retain operation key and inspect before any new review")
	}
	return json.NewEncoder(out).Encode(result)
}
