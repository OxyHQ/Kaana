package main

import (
	"errors"
	"flag"
	"io"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
)

const destroyUsage = "usage: kaana-credentials destroy --operation-id <kcd_id> --provider <slug> --key-id <exact-id>"

// parseDestroyOperation accepts exact selectors only. There is no force flag:
// a key is destroyed only when credentialstore.Destroy proves no working
// credential is lost (a duplicate of an enabled key, or rejected by its
// provider).
func parseDestroyOperation(arguments []string, actor string) (credentialstore.DestroyOperation, error) {
	flags := flag.NewFlagSet("destroy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	operationID := flags.String("operation-id", "", "kcd_ operation id")
	providerSlug := flags.String("provider", "", "exact provider slug")
	keyID := flags.String("key-id", "", "exact disabled key id")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return credentialstore.DestroyOperation{}, errors.New(destroyUsage)
	}
	operation := credentialstore.DestroyOperation{
		OperationID: *operationID,
		Scope:       credentialstore.Scope{Provider: contract.ProviderSlug(*providerSlug), KeyID: *keyID},
		Actor:       actor,
	}
	if err := credentialstore.ValidateDestroyOperation(operation); err != nil {
		return credentialstore.DestroyOperation{}, err
	}
	return operation, nil
}
