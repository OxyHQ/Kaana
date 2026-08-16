// Package inventory holds Relay's deployment inventory: which provider serves
// which model reference, and what that provider calls the model.
//
// This is the one piece of catalogue knowledge the data plane genuinely owns.
// ADR 0006 assigns deployment availability to Relay and model identity and
// pricing to Oxy, and the published catalogue's deployment descriptor carries
// no field for a provider's own model identifier — so the mapping from
// `openai/gpt-5@2026-05-01` to whatever string OpenAI expects exists nowhere in
// the contract and has to live here.
//
// It deliberately holds nothing else. No account, no application, no
// credential, no price, no commercial permission: a Relay table keyed by an Oxy
// id is a copy of an Oxy entity, and this file is where that temptation would
// first appear.
package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/OxyHQ/Relay/internal/contract"
	"github.com/OxyHQ/Relay/internal/provider"
)

// Deployment is one concrete servable route.
type Deployment struct {
	DeploymentID contract.DeploymentID `json:"deploymentId"`
	Provider     contract.ProviderSlug `json:"provider"`
	// ModelReference is always revision-pinned: a deployment serves specific
	// weights.
	ModelReference contract.ModelReference `json:"modelReference"`
	// UpstreamModelID is what the provider's own API calls this model.
	UpstreamModelID string          `json:"upstreamModelId"`
	Region          contract.Region `json:"region"`
	// Current marks the revision an UNPINNED reference to this model line
	// resolves to.
	//
	// Choosing the current revision of a model is described in the contract as
	// Oxy's decision, but the envelope carries no resolution and the stream's
	// start event must report a revision-pinned reference — so in practice the
	// choice lands here. See README, "What Oxy still has to decide".
	Current bool `json:"current"`
}

// Inventory resolves a model reference to the route that serves it.
type Inventory struct {
	byReference map[contract.ModelReference]Deployment
	currentOf   map[contract.ModelID]Deployment
}

// ErrNoRoute is returned when nothing in the inventory serves a reference. The
// caller maps it to the contract's `model_not_found`, which is non-retryable:
// an identical retry cannot make a route appear.
type ErrNoRoute struct {
	Reference contract.ModelReference
}

func (e ErrNoRoute) Error() string {
	return fmt.Sprintf("inventory: no deployment serves %q", e.Reference)
}

type file struct {
	Deployments []Deployment `json:"deployments"`
}

// Load reads an inventory from a JSON file.
func Load(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("inventory: reading %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse builds an inventory, refusing anything a route resolution would later
// have to guess about.
//
// Every refusal here is a failure that would otherwise surface as a served
// request: an unpinned deployment silently serving whatever the provider's
// alias points at today, two "current" revisions resolving differently per
// process start, an empty upstream model id producing a 404 from the provider
// that reads like an outage.
func Parse(raw []byte) (*Inventory, error) {
	var parsed file
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	if len(parsed.Deployments) == 0 {
		return nil, fmt.Errorf("inventory: no deployments declared; nothing could be served")
	}

	inventory := &Inventory{
		byReference: make(map[contract.ModelReference]Deployment, len(parsed.Deployments)),
		currentOf:   make(map[contract.ModelID]Deployment),
	}
	seenIDs := make(map[contract.DeploymentID]struct{}, len(parsed.Deployments))

	for _, deployment := range parsed.Deployments {
		switch {
		case deployment.DeploymentID == "":
			return nil, fmt.Errorf("inventory: a deployment has no id")
		case !deployment.Provider.Valid():
			return nil, fmt.Errorf("inventory: %s has provider %q, which is not a provider slug", deployment.DeploymentID, deployment.Provider)
		case !deployment.ModelReference.Valid():
			return nil, fmt.Errorf("inventory: %s has model reference %q, which is not a model reference", deployment.DeploymentID, deployment.ModelReference)
		case !deployment.ModelReference.Pinned():
			return nil, fmt.Errorf("inventory: %s must pin an immutable revision (<publisher>/<model>@<revision>), got %q", deployment.DeploymentID, deployment.ModelReference)
		case deployment.UpstreamModelID == "":
			return nil, fmt.Errorf("inventory: %s declares no upstream model id, so nothing could be sent to %s", deployment.DeploymentID, deployment.Provider)
		case deployment.Region != "" && !deployment.Region.Valid():
			return nil, fmt.Errorf("inventory: %s has region %q, which is not a region", deployment.DeploymentID, deployment.Region)
		}

		if _, duplicate := seenIDs[deployment.DeploymentID]; duplicate {
			return nil, fmt.Errorf("inventory: two deployments share the id %s", deployment.DeploymentID)
		}
		seenIDs[deployment.DeploymentID] = struct{}{}

		if _, duplicate := inventory.byReference[deployment.ModelReference]; duplicate {
			// Same-model failover, which would make this legitimate, is
			// explicitly out of scope for this build. Accepting a second
			// deployment now would mean picking one silently.
			return nil, fmt.Errorf("inventory: %q has two deployments; same-model failover is not implemented, so the choice would be arbitrary", deployment.ModelReference)
		}
		inventory.byReference[deployment.ModelReference] = deployment

		if deployment.Current {
			line := deployment.ModelReference.ModelID()
			if existing, duplicate := inventory.currentOf[line]; duplicate {
				return nil, fmt.Errorf("inventory: %s and %s are both current for %q", existing.DeploymentID, deployment.DeploymentID, line)
			}
			inventory.currentOf[line] = deployment
		}
	}
	return inventory, nil
}

// Resolve returns the route serving a model reference.
//
// A pinned reference resolves to exactly that revision or to nothing: a request
// that named specific weights is served or refused, never substituted. An
// unpinned reference resolves to the model line's current revision.
func (i *Inventory) Resolve(reference contract.ModelReference) (provider.Route, error) {
	deployment, found := i.byReference[reference]
	if !found {
		if reference.Pinned() {
			return provider.Route{}, ErrNoRoute{Reference: reference}
		}
		deployment, found = i.currentOf[reference.ModelID()]
		if !found {
			return provider.Route{}, ErrNoRoute{Reference: reference}
		}
	}
	return provider.Route{
		DeploymentID:    deployment.DeploymentID,
		Provider:        deployment.Provider,
		ModelReference:  deployment.ModelReference,
		UpstreamModelID: deployment.UpstreamModelID,
		Region:          deployment.Region,
	}, nil
}

// Providers lists the provider slugs the inventory routes to, sorted. The
// server uses it to refuse at startup if a routable provider has no adapter,
// rather than discovering it on a customer request.
func (i *Inventory) Providers() []contract.ProviderSlug {
	seen := make(map[contract.ProviderSlug]struct{})
	for _, deployment := range i.byReference {
		seen[deployment.Provider] = struct{}{}
	}
	slugs := make([]contract.ProviderSlug, 0, len(seen))
	for slug := range seen {
		slugs = append(slugs, slug)
	}
	sort.Slice(slugs, func(a, b int) bool { return slugs[a] < slugs[b] })
	return slugs
}
