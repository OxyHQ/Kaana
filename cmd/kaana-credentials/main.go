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
	mutationActor := strings.TrimSpace(getenv("KAANA_CREDENTIAL_ACTOR"))
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
		keyID := flags.String("key-id", "", "operator-facing key id")
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
		store, repository, err := credentialstore.Open(ctx, databaseURL, strings.TrimSpace(getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN")))
		if err != nil {
			return err
		}
		defer repository.Close()
		input := credentialstore.EncryptedCredential{
			Scope: credentialstore.Scope{
				Provider: contract.ProviderSlug(strings.TrimSpace(*providerSlug)),
				KeyID:    strings.TrimSpace(*keyID),
			},
			Class:     provider.KeyClass(strings.TrimSpace(*class)),
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
		keyID := flags.String("key-id", "", "operator-facing key id")
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
		providerSlugValue := contract.ProviderSlug(strings.TrimSpace(*providerSlug))
		secret, err := source.ReadSecureString(ctx, *parameter, providerSlugValue)
		if err != nil {
			return err
		}
		defer clear(secret)
		store, repository, err := credentialstore.Open(ctx, databaseURL, strings.TrimSpace(getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN")))
		if err != nil {
			return err
		}
		defer repository.Close()
		input := credentialstore.EncryptedCredential{
			Scope: credentialstore.Scope{
				Provider: providerSlugValue,
				KeyID:    strings.TrimSpace(*keyID),
			},
			Class:     provider.KeyClass(strings.TrimSpace(*class)),
			BudgetUSD: budgetValue,
			Position:  *position,
		}
		if err := store.Put(ctx, input, secret, mutationActor); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "imported %s/%s\n", input.Provider, input.KeyID)
		return err

	case "disable":
		flags := flag.NewFlagSet("disable", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		providerSlug := flags.String("provider", "", "provider slug")
		keyID := flags.String("key-id", "", "operator-facing key id")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("usage: kaana-credentials disable --provider <slug> --key-id <id>")
		}
		store, repository, err := credentialstore.Open(ctx, databaseURL, strings.TrimSpace(getenv("KAANA_PROVIDER_CREDENTIALS_KMS_KEY_ARN")))
		if err != nil {
			return err
		}
		defer repository.Close()
		scope := credentialstore.Scope{Provider: contract.ProviderSlug(strings.TrimSpace(*providerSlug)), KeyID: strings.TrimSpace(*keyID)}
		changed, err := store.Disable(ctx, scope, mutationActor)
		if err != nil {
			return err
		}
		if !changed {
			return fmt.Errorf("credential %s/%s is absent or already disabled", scope.Provider, scope.KeyID)
		}
		_, err = fmt.Fprintf(stdout, "disabled %s/%s\n", scope.Provider, scope.KeyID)
		return err

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
	return errors.New("usage: kaana-credentials <migrate|put|import-ssm|disable|list>")
}
