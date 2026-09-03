// Command kaana-credentials administers Kaana's encrypted provider keys.
//
// Secret input is accepted only on stdin. A provider key in argv is retained
// by shell history and process inspection; a key in an environment variable is
// inherited by child processes and projected by deployment tooling. Neither is
// a credential transport this command supports.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const maxCredentialBytes = 4096

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, stdin io.Reader, stdout io.Writer, getenv func(string) string) error {
	if len(arguments) == 0 {
		return usageError()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	databaseURL := strings.TrimSpace(getenv("DATABASE_URL"))
	mutationActor := getenv("KAANA_CREDENTIAL_ACTOR")
	switch arguments[0] {
	case "migrate":
		flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: kaana-credentials migrate")
		}
		repository, err := credentialstore.OpenPostgres(ctx, databaseURL)
		if err != nil {
			return err
		}
		defer repository.Close()
		if err := repository.Migrate(ctx); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, "Kaana credential schema is current")
		return err

	case "put":
		flags := flag.NewFlagSet("put", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		providerSlug := flags.String("provider", "", "provider slug")
		keyID := flags.String("key-id", "", "exact opaque key id")
		class := flags.String("class", "", "free, paid, or empty")
		budget := flags.String("budget-usd", "", "optional budget metadata")
		position := flags.Int("position", 0, "1-based pool order")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: kaana-credentials put --provider <slug> --key-id <id> --position <n> [--class free|paid] [--budget-usd <amount>] < secret")
		}
		secret, err := readCredential(stdin)
		if err != nil {
			return err
		}
		defer clear(secret)
		budgetValue, err := parseBudget(*budget)
		if err != nil {
			return err
		}
		store, repository, err := credentialstore.Open(ctx, databaseURL, getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN"))
		if err != nil {
			return err
		}
		defer repository.Close()
		input := credentialstore.EncryptedCredential{
			Scope: credentialstore.Scope{
				Provider: contract.ProviderSlug(*providerSlug),
				KeyID:    *keyID,
			},
			Class:     provider.KeyClass(*class),
			BudgetUSD: budgetValue,
			Position:  *position,
		}
		if err := store.Put(ctx, input, secret, mutationActor); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "saved %s/%s\n", input.Provider, input.KeyID)
		return err

	case "import-ssm":
		flags := flag.NewFlagSet("import-ssm", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		providerSlug := flags.String("provider", "", "provider slug")
		keyID := flags.String("key-id", "", "exact opaque key id")
		parameter := flags.String("parameter", "", "legacy SecureString path")
		class := flags.String("class", "", "free, paid, or empty")
		budget := flags.String("budget-usd", "", "optional budget metadata")
		position := flags.Int("position", 0, "1-based pool order")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: kaana-credentials import-ssm --provider <slug> --key-id <id> --position <n> --parameter </path> [--class free|paid] [--budget-usd <amount>]")
		}
		budgetValue, err := parseBudget(*budget)
		if err != nil {
			return err
		}
		source, err := credentialstore.OpenSSMSource(ctx)
		if err != nil {
			return err
		}
		providerSlugValue := contract.ProviderSlug(*providerSlug)
		scope := credentialstore.Scope{Provider: providerSlugValue, KeyID: *keyID}
		secret, err := source.ReadSecureString(ctx, *parameter, scope)
		if err != nil {
			return err
		}
		defer clear(secret)
		store, repository, err := credentialstore.Open(ctx, databaseURL, getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN"))
		if err != nil {
			return err
		}
		defer repository.Close()
		input := credentialstore.EncryptedCredential{
			Scope:     scope,
			Class:     provider.KeyClass(*class),
			BudgetUSD: budgetValue,
			Position:  *position,
		}
		if err := store.ImportLegacy(ctx, input, secret, mutationActor); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "imported %s/%s\n", input.Provider, input.KeyID)
		return err

	case "disable":
		flags := flag.NewFlagSet("disable", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		providerSlug := flags.String("provider", "", "provider slug")
		keyID := flags.String("key-id", "", "exact opaque key id")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: kaana-credentials disable --provider <slug> --key-id <id>")
		}
		store, repository, err := credentialstore.Open(ctx, databaseURL, getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN"))
		if err != nil {
			return err
		}
		defer repository.Close()
		scope := credentialstore.Scope{Provider: contract.ProviderSlug(*providerSlug), KeyID: *keyID}
		changed, err := store.Disable(ctx, scope, mutationActor)
		if err != nil {
			return err
		}
		if !changed {
			return fmt.Errorf("credential %s/%s is absent or already disabled", scope.Provider, scope.KeyID)
		}
		_, err = fmt.Fprintf(stdout, "disabled %s/%s\n", scope.Provider, scope.KeyID)
		return err

	case "rekey-id":
		operation, err := parseRekeyOperation(arguments[1:], mutationActor)
		if err != nil {
			return err
		}
		store, repository, err := credentialstore.Open(ctx, databaseURL, getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN"))
		if err != nil {
			return err
		}
		defer repository.Close()
		receipt, err := store.RekeyID(ctx, operation)
		if err != nil {
			return err
		}
		return writeReceipt(stdout, receipt)

	case "deduplicate":
		operation, err := parseDeduplicationOperation(arguments[1:], mutationActor)
		if err != nil {
			return err
		}
		store, repository, err := credentialstore.Open(ctx, databaseURL, getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN"))
		if err != nil {
			return err
		}
		defer repository.Close()
		receipt, err := store.Deduplicate(ctx, operation)
		if err != nil {
			return err
		}
		return writeReceipt(stdout, receipt)

	case "list":
		flags := flag.NewFlagSet("list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: kaana-credentials list")
		}
		// Listing needs PostgreSQL only. It intentionally does not initialize KMS
		// because ciphertext is not selected or returned by this operation.
		repository, err := credentialstore.OpenPostgres(ctx, databaseURL)
		if err != nil {
			return err
		}
		defer repository.Close()
		metadata, err := repository.ListMetadata(ctx)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(metadata)
	default:
		return usageError()
	}
}

func parseRekeyOperation(arguments []string, actor string) (credentialstore.CredentialIDOperation, error) {
	flags := flag.NewFlagSet("rekey-id", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	operationID := flags.String("operation-id", "", "exact idempotency id")
	providerSlug := flags.String("provider", "", "provider slug")
	oldKeyID := flags.String("old-key-id", "", "exact existing key id")
	newKeyID := flags.String("new-key-id", "", "exact opaque UUIDv4 key id")
	prerequisiteOperationID := flags.String("requires-operation-id", "", "exact prerequisite receipt id")
	prerequisiteOutcome := flags.String("requires-outcome", "", "optional exact prerequisite outcome")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return credentialstore.CredentialIDOperation{}, errors.New("usage: kaana-credentials rekey-id --operation-id <kop_id> --provider <slug> --old-key-id <exact-id> --new-key-id <opaque-uuid> [--requires-operation-id <kop_id> [--requires-outcome <outcome>]]")
	}
	operation := credentialstore.CredentialIDOperation{
		OperationID:             *operationID,
		Provider:                contract.ProviderSlug(*providerSlug),
		SourceKeyID:             *oldKeyID,
		DestinationKeyID:        *newKeyID,
		PrerequisiteOperationID: *prerequisiteOperationID,
		PrerequisiteOutcome:     credentialstore.CredentialAdminOutcome(*prerequisiteOutcome),
		Actor:                   actor,
	}
	if err := credentialstore.ValidateRekeyIDOperation(operation); err != nil {
		return credentialstore.CredentialIDOperation{}, err
	}
	return operation, nil
}

func parseDeduplicationOperation(arguments []string, actor string) (credentialstore.CredentialIDOperation, error) {
	flags := flag.NewFlagSet("deduplicate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	operationID := flags.String("operation-id", "", "exact idempotency id")
	providerSlug := flags.String("provider", "", "provider slug")
	duplicateKeyID := flags.String("duplicate-key-id", "", "exact candidate duplicate id")
	keepKeyID := flags.String("keep-key-id", "", "exact id to preserve")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return credentialstore.CredentialIDOperation{}, errors.New("usage: kaana-credentials deduplicate --operation-id <kop_id> --provider <slug> --duplicate-key-id <exact-id> --keep-key-id <exact-id>")
	}
	operation := credentialstore.CredentialIDOperation{
		OperationID:      *operationID,
		Provider:         contract.ProviderSlug(*providerSlug),
		SourceKeyID:      *duplicateKeyID,
		DestinationKeyID: *keepKeyID,
		Actor:            actor,
	}
	if err := credentialstore.ValidateDeduplicationOperation(operation); err != nil {
		return credentialstore.CredentialIDOperation{}, err
	}
	return operation, nil
}

func writeReceipt(output io.Writer, receipt credentialstore.CredentialAdminReceipt) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(receipt)
}

func readCredential(input io.Reader) ([]byte, error) {
	secret, err := io.ReadAll(io.LimitReader(input, maxCredentialBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading credential from stdin: %w", err)
	}
	if len(secret) > maxCredentialBytes {
		clear(secret)
		return nil, fmt.Errorf("provider credential exceeds %d bytes", maxCredentialBytes)
	}
	secret = trimOneLineEnding(secret)
	if len(secret) == 0 {
		return nil, errors.New("provider credential must be supplied on stdin")
	}
	if bytes.ContainsAny(secret, "\r\n") {
		clear(secret)
		return nil, errors.New("provider credential must be one line")
	}
	return secret, nil
}

func trimOneLineEnding(value []byte) []byte {
	if len(value) > 0 && value[len(value)-1] == '\n' {
		value = value[:len(value)-1]
		if len(value) > 0 && value[len(value)-1] == '\r' {
			value = value[:len(value)-1]
		}
	}
	return value
}

func parseBudget(raw string) (*float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || value < 0 {
		return nil, fmt.Errorf("budget-usd must be a non-negative number, got %q", raw)
	}
	return &value, nil
}

func usageError() error {
	return errors.New("usage: kaana-credentials <migrate|put|import-ssm|disable|rekey-id|deduplicate|list>")
}
