package contract

import (
	"encoding/json"
	"fmt"
)

// UsageReport is the data plane's technical account of one request — the only
// usage shape Kaana produces.
//
// No money and no price appear on it. Kaana measures units and names the route
// it used; the control plane decides what that costs. Keeping the two apart is
// what allows a price to be corrected after the fact without re-running or
// re-measuring anything, and it is the reason there is no Money type in this
// package at all.
type UsageReport struct {
	SchemaVersion          int             `json:"schemaVersion"`
	RequestID              RequestID       `json:"requestId"`
	GenerationID           *GenerationID   `json:"generationId,omitempty"`
	Attribution            Attribution     `json:"attribution"`
	Outcome                RequestOutcome  `json:"outcome"`
	Units                  []UsageQuantity `json:"units"`
	UsageSource            UsageSource     `json:"usageSource"`
	ResolvedModelReference ModelReference  `json:"resolvedModelReference"`
	ServingProvider        ProviderSlug    `json:"servingProvider"`
	DeploymentID           DeploymentID    `json:"deploymentId"`
	RouteSwitches          int             `json:"routeSwitches"`
	StartedAt              Timestamp       `json:"startedAt"`
	CompletedAt            Timestamp       `json:"completedAt"`
	TimeToFirstTokenMs     *int            `json:"timeToFirstTokenMs,omitempty"`
}

// MarshalJSON renders an unmeasured request as `"units": []`.
//
// The contract declares `units` as an array that is neither nullable nor
// optional, and its one representation of "nothing was measured" is the empty
// list. Go's zero slice is nil, `encoding/json` renders nil as `null`, and a
// failed generation is exactly the case that leaves Units unassigned — so
// without this the reports Oxy rejects are precisely the ones that report a
// provider refusing the request.
//
// It lives on the type rather than at the one place the executor builds a
// report, because the wire shape is a property of the shape and not of any
// construction site. A future second producer, a test fixture and a replay all
// go through this.
func (r UsageReport) MarshalJSON() ([]byte, error) {
	// A distinct type so this method is not in scope inside itself; the field
	// set and every tag are still UsageReport's.
	type wire UsageReport
	encoded := wire(r)
	if encoded.Units == nil {
		encoded.Units = []UsageQuantity{}
	}
	return json.Marshal(encoded)
}

// Validate applies the refinements the published schema carries, so a report
// Kaana would emit and Oxy would reject is caught on this side of the wire.
//
// A rejected usage report is not a lost log line: it is the record settlement
// runs against, so the failure mode of emitting an invalid one is a request
// that executed, cost money upstream, and can never be settled or refunded.
func (r *UsageReport) Validate() error {
	if r.SchemaVersion != UsageReportSchemaVersion {
		return fmt.Errorf("contract: usage report schemaVersion must be %d", UsageReportSchemaVersion)
	}
	if r.RequestID == "" {
		return fmt.Errorf("contract: usage report requestId is required")
	}
	if !isMember(r.Outcome, requestOutcomeValues) {
		return fmt.Errorf("contract: %q is not a request outcome", r.Outcome)
	}
	if !isMember(r.UsageSource, usageSourceValues) {
		return fmt.Errorf("contract: %q is not a usage source", r.UsageSource)
	}
	if !r.ResolvedModelReference.Valid() {
		return fmt.Errorf("contract: %q is not a model reference", r.ResolvedModelReference)
	}
	if !r.ServingProvider.Valid() {
		return fmt.Errorf("contract: %q is not a provider slug", r.ServingProvider)
	}
	if len(r.DeploymentID) == 0 || len(r.DeploymentID) > 128 {
		return fmt.Errorf("contract: deploymentId must contain between 1 and 128 characters")
	}
	if r.RouteSwitches < 0 || r.RouteSwitches > 100 {
		return fmt.Errorf("contract: routeSwitches %d is outside the contract's range", r.RouteSwitches)
	}
	if r.Outcome == OutcomeCompleted && len(r.Units) == 0 {
		return fmt.Errorf("contract: a completed request must report at least one usage unit")
	}
	seen := make(map[UsageUnit]struct{}, len(r.Units))
	for _, quantity := range r.Units {
		if !isMember(quantity.Unit, usageUnitValues) {
			return fmt.Errorf("contract: %q is not a usage unit", quantity.Unit)
		}
		if quantity.Quantity < 0 {
			return fmt.Errorf("contract: %s quantity is negative", quantity.Unit)
		}
		if _, duplicate := seen[quantity.Unit]; duplicate {
			return fmt.Errorf("contract: each unit is reported once, as a total (%s repeats)", quantity.Unit)
		}
		seen[quantity.Unit] = struct{}{}
	}
	started, err := r.StartedAt.Time()
	if err != nil {
		return fmt.Errorf("contract: startedAt: %w", err)
	}
	completed, err := r.CompletedAt.Time()
	if err != nil {
		return fmt.Errorf("contract: completedAt: %w", err)
	}
	if completed.Before(started) {
		return fmt.Errorf("contract: a request cannot complete before it started")
	}
	return nil
}
