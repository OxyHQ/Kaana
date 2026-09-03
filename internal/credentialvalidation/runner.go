// Package credentialvalidation runs the separately authenticated bootstrap
// probe for one quarantined customer credential generation.
package credentialvalidation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/credentialstore"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/oxyvalidation"
	"github.com/OxyHQ/Kaana/internal/provider"
)

const (
	defaultProbeTimeout = 20 * time.Second
	// The PostgreSQL claim lease is 60 seconds. Keeping the complete resolver,
	// translation and provider call below 45 seconds leaves a fixed completion
	// margin and prevents an operator setting that lets another worker reclaim
	// and repeat the upstream probe while the first worker is still running.
	maxProbeTimeout = 45 * time.Second
)

// ErrOperationConflict is intentionally selector-free. It means the Oxy
// operation id already names another exact validation task.
var ErrOperationConflict = errors.New("credential validation: operation identity conflicts")

// Repository is the validation-only PostgreSQL authority. Production exposes
// it through SECURITY DEFINER functions; it grants no table DML.
type Repository interface {
	ClaimCustomerValidation(context.Context, credentialstore.CustomerCredentialValidationOperation) (credentialstore.CustomerCredentialValidationOutcome, error)
	CompleteCustomerValidation(context.Context, credentialstore.CustomerCredentialValidationOutcome) error
}

// CredentialResolver is the runtime's existing exact-generation decrypt
// authority. No new decryption surface is introduced.
type CredentialResolver interface {
	ResolveForInference(context.Context, credentialstore.CustomerCredentialScope) ([]byte, error)
}

// Reporter sends the durable outcome back under Kaana's narrow Oxy service
// principal. Replaying a terminal operation submits the same exact outcome.
type Reporter interface {
	Submit(oxyvalidation.Verdict)
}

// Runner executes no customer request and produces no usage report, receipt,
// breaker observation, failover or user-visible output.
type Runner struct {
	repository   Repository
	resolver     CredentialResolver
	inventory    *inventory.Store
	providers    *provider.Registry
	reporter     Reporter
	probeTimeout time.Duration
}

// Config wires the complete bootstrap boundary.
type Config struct {
	Repository   Repository
	Resolver     CredentialResolver
	Inventory    *inventory.Store
	Providers    *provider.Registry
	Reporter     Reporter
	ProbeTimeout time.Duration
}

// New refuses a partially configured validation path.
func New(config Config) (*Runner, error) {
	switch {
	case config.Repository == nil:
		return nil, errors.New("credential validation: repository is required")
	case config.Resolver == nil:
		return nil, errors.New("credential validation: credential resolver is required")
	case config.Inventory == nil:
		return nil, errors.New("credential validation: inventory is required")
	case config.Providers == nil:
		return nil, errors.New("credential validation: provider registry is required")
	case config.Reporter == nil:
		return nil, errors.New("credential validation: Oxy outcome reporter is required")
	}
	timeout := config.ProbeTimeout
	if timeout == 0 {
		timeout = defaultProbeTimeout
	}
	if timeout < 0 || timeout > maxProbeTimeout {
		return nil, fmt.Errorf("credential validation: probe timeout must be positive and not exceed %s", maxProbeTimeout)
	}
	return &Runner{
		repository: config.Repository, resolver: config.Resolver,
		inventory: config.Inventory, providers: config.Providers,
		reporter: config.Reporter, probeTimeout: timeout,
	}, nil
}

// Validate claims or replays one durable operation. Only a successful real
// call on the exact deployment produces valid. Only an explicit provider
// authentication refusal proves invalidity. Billing, quota and every other
// failure are inconclusive and leave Oxy's generation quarantined so the same
// key can be revalidated after the upstream account is funded.
func (r *Runner) Validate(ctx context.Context, task contract.KaanaCredentialValidationTask) (contract.KaanaCredentialValidationOutcome, error) {
	if err := validateTask(task); err != nil {
		return contract.KaanaCredentialValidationOutcome{}, err
	}
	operation := operationFromTask(task)
	claimed, err := r.repository.ClaimCustomerValidation(ctx, operation)
	if err != nil {
		return contract.KaanaCredentialValidationOutcome{}, fmt.Errorf("credential validation: claiming operation: %w", err)
	}
	switch claimed.State {
	case credentialstore.CustomerCredentialValidationConflict:
		return contract.KaanaCredentialValidationOutcome{}, ErrOperationConflict
	case credentialstore.CustomerCredentialValidationPending:
		return outcomeFor(task, claimed), nil
	case credentialstore.CustomerCredentialValidationValid,
		credentialstore.CustomerCredentialValidationInvalid,
		credentialstore.CustomerCredentialValidationInconclusive:
		outcome := outcomeFor(task, claimed)
		r.submit(outcome)
		return outcome, nil
	case credentialstore.CustomerCredentialValidationExecute:
		// This process owns the durable lease and performs the one real probe.
		if claimed.LeaseGeneration <= 0 {
			return contract.KaanaCredentialValidationOutcome{}, errors.New("credential validation: repository returned an invalid lease")
		}
	default:
		return contract.KaanaCredentialValidationOutcome{}, errors.New("credential validation: repository returned an invalid state")
	}

	result := r.probe(ctx, operation)
	result.LeaseGeneration = claimed.LeaseGeneration
	if err := r.repository.CompleteCustomerValidation(ctx, result); err != nil {
		return contract.KaanaCredentialValidationOutcome{}, fmt.Errorf("credential validation: committing outcome: %w", err)
	}
	outcome := outcomeFor(task, result)
	r.submit(outcome)
	return outcome, nil
}

func (r *Runner) probe(parent context.Context, operation credentialstore.CustomerCredentialValidationOperation) credentialstore.CustomerCredentialValidationOutcome {
	ctx, cancel := context.WithTimeout(parent, r.probeTimeout)
	defer cancel()
	inconclusive := func(code credentialstore.CustomerCredentialValidationFailure) credentialstore.CustomerCredentialValidationOutcome {
		return credentialstore.CustomerCredentialValidationOutcome{Operation: operation, State: credentialstore.CustomerCredentialValidationInconclusive, FailureCode: code}
	}
	if ctx.Err() != nil {
		return inconclusive(credentialstore.CustomerCredentialValidationNetwork)
	}
	route, err := r.inventory.Current().Deployment(operation.DeploymentID)
	if err != nil || route.Provider != operation.Provider {
		return inconclusive(credentialstore.CustomerCredentialValidationNotFound)
	}
	adapter, found := r.providers.Lookup(operation.Provider)
	if !found {
		return inconclusive(credentialstore.CustomerCredentialValidationNotFound)
	}
	plaintext, err := r.resolver.ResolveForInference(ctx, operation.CustomerCredentialScope)
	if err != nil {
		clear(plaintext)
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return inconclusive(credentialstore.CustomerCredentialValidationNetwork)
		}
		return inconclusive(credentialstore.CustomerCredentialValidationNotFound)
	}
	pool, err := provider.NewCustomerKeyPool(operation.Provider, plaintext)
	clear(plaintext)
	if err != nil {
		return inconclusive(credentialstore.CustomerCredentialValidationUnknown)
	}
	defer pool.Destroy()

	probeText := "."
	maxOutputTokens := 1
	request := &contract.Request{
		Modality: contract.ModalityText,
		Input: contract.Input{Format: contract.InputMessages, Messages: []contract.Message{{
			Role:    contract.RoleUser,
			Content: []contract.ContentPart{{Type: contract.ContentPartText, Text: &probeText}},
		}}},
		Stream:          false,
		MaxOutputTokens: &maxOutputTokens,
	}
	call, err := adapter.Translate(request, route)
	if err != nil {
		return inconclusive(credentialstore.CustomerCredentialValidationUnknown)
	}
	if ctx.Err() != nil {
		return inconclusive(credentialstore.CustomerCredentialValidationNetwork)
	}
	_, err = adapter.Stream(ctx, call, discardEmitter{}, pool)
	if err == nil {
		return credentialstore.CustomerCredentialValidationOutcome{Operation: operation, State: credentialstore.CustomerCredentialValidationValid}
	}
	var customerFailure provider.ErrCustomerCredential
	if errors.As(err, &customerFailure) {
		switch customerFailure.Code {
		case contract.CodeProviderCredentialInvalid:
			return credentialstore.CustomerCredentialValidationOutcome{Operation: operation, State: credentialstore.CustomerCredentialValidationInvalid, FailureCode: credentialstore.CustomerCredentialValidationUnauthorized}
		case contract.CodeProviderBillingRefused:
			return inconclusive(credentialstore.CustomerCredentialValidationForbidden)
		}
	}
	var upstream provider.ErrUpstream
	if errors.As(err, &upstream) && upstream.Code == contract.CodeRateLimited {
		return inconclusive(credentialstore.CustomerCredentialValidationRateLimited)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return inconclusive(credentialstore.CustomerCredentialValidationNetwork)
	}
	return inconclusive(credentialstore.CustomerCredentialValidationUnknown)
}

func (r *Runner) submit(outcome contract.KaanaCredentialValidationOutcome) {
	failureCode := oxyvalidation.FailureCode("")
	if outcome.FailureCode != nil {
		failureCode = oxyvalidation.FailureCode(*outcome.FailureCode)
	}
	r.reporter.Submit(oxyvalidation.Verdict{
		OperationID: outcome.OperationID, ApplicationID: outcome.ApplicationID,
		Provider: outcome.Provider, OwnerAccountID: outcome.OwnerAccountID,
		ConnectionID: outcome.ConnectionID, CredentialHandle: string(outcome.CredentialHandle),
		CredentialRevision: outcome.CredentialRevision, Environment: outcome.Environment,
		DeploymentID: outcome.DeploymentID, State: oxyvalidation.State(outcome.State),
		FailureCode: failureCode,
	})
}

func operationFromTask(task contract.KaanaCredentialValidationTask) credentialstore.CustomerCredentialValidationOperation {
	return credentialstore.CustomerCredentialValidationOperation{
		OperationID: string(task.OperationID), ApplicationID: string(task.ApplicationID),
		CustomerCredentialScope: credentialstore.CustomerCredentialScope{
			CustomerCredentialIdentity: credentialstore.CustomerCredentialIdentity{
				Provider: task.Provider, OwnerAccountID: string(task.OwnerAccountID),
				ConnectionID: task.ConnectionID, Environment: task.Environment,
			},
			CredentialHandle: string(task.CredentialHandle), Revision: task.CredentialRevision,
		},
		DeploymentID: task.DeploymentID,
	}
}

func outcomeFor(task contract.KaanaCredentialValidationTask, result credentialstore.CustomerCredentialValidationOutcome) contract.KaanaCredentialValidationOutcome {
	var failureCode *contract.KaanaCredentialValidationFailureCode
	if result.FailureCode != "" {
		value := contract.KaanaCredentialValidationFailureCode(result.FailureCode)
		failureCode = &value
	}
	return contract.KaanaCredentialValidationOutcome{
		SchemaVersion: task.SchemaVersion, OperationID: task.OperationID,
		ApplicationID: task.ApplicationID, Provider: task.Provider,
		OwnerAccountID: task.OwnerAccountID, ConnectionID: task.ConnectionID,
		Environment: task.Environment, CredentialHandle: task.CredentialHandle,
		CredentialRevision: task.CredentialRevision, DeploymentID: task.DeploymentID,
		State: contract.KaanaCredentialValidationOutcomeState(result.State), FailureCode: failureCode,
	}
}

func validateTask(task contract.KaanaCredentialValidationTask) error {
	if task.SchemaVersion != 1 || !validOpaque(string(task.OperationID), 128) ||
		!validOpaque(string(task.ApplicationID), 64) || !task.Provider.Valid() ||
		!validOpaque(string(task.OwnerAccountID), 64) || !validOpaque(task.ConnectionID, 128) ||
		!validHandle(string(task.CredentialHandle)) || task.CredentialRevision <= 0 ||
		task.CredentialRevision > 1<<53-1 || len(task.DeploymentID) < 1 || len(task.DeploymentID) > 128 {
		return errors.New("credential validation: task is invalid")
	}
	switch task.Environment {
	case contract.EnvironmentDevelopment, contract.EnvironmentStaging, contract.EnvironmentProduction:
		return nil
	default:
		return errors.New("credential validation: task is invalid")
	}
}

func validOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validHandle(handle string) bool {
	if len(handle) != len("kcred_")+26 || handle[:len("kcred_")] != "kcred_" {
		return false
	}
	for _, character := range handle[len("kcred_"):] {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			return false
		}
	}
	return true
}

type discardEmitter struct{}

func (discardEmitter) Start(contract.ModelReference, time.Time) error { return nil }
func (discardEmitter) Delta(int, contract.DeltaChannel, string) error { return nil }
func (discardEmitter) ToolCall(provider.ToolCallDelta) error          { return nil }
func (discardEmitter) Usage([]contract.UsageQuantity, contract.UsageSource) error {
	return nil
}
