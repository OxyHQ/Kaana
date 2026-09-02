// Package inventory holds Kaana's deployment inventory: which providers serve
// which model reference, and what each of them calls the model.
//
// This is the one piece of catalogue knowledge the data plane genuinely owns.
// ADR 0006 assigns deployment availability to Kaana and model identity and
// pricing to Oxy, and the published catalogue's deployment descriptor carries
// no field for a provider's own model identifier — so the mapping from
// `openai/gpt-5@2026-05-01` to whatever string OpenAI expects exists nowhere in
// the contract and has to live here.
//
// It deliberately holds nothing else. No account, no application, no
// credential, no price, no commercial permission: a Kaana table keyed by an Oxy
// id is a copy of an Oxy entity, and this file is where that temptation would
// first appear. The upstream rate cards that price a PROVIDER's invoice are a
// separate file read by a separate package, so that nothing on the path from a
// customer request to a served route can reach an amount at all.
//
// # Why one reference owns many endpoints
//
// A model reference resolves to a RouteSet: one reference and the endpoints
// that serve it. The reference is stored once, for the whole set, and an
// Endpoint carries none of its own. That shape guarantees that every route
// built from this set serves the same exact weights. Cross-model execution is
// a separate executor operation: it resolves another RouteSet only for an
// explicit entry in the signed authorizedRoutes list and reports that change
// as a model-scoped route switch.
package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
)

// DefaultMaxSnapshotAge is how long an inventory snapshot may go without being
// re-issued before Kaana stops resolving UNPINNED references from it.
//
// It is generous on purpose. A pinned reference is served from a snapshot of
// any age — the mapping from immutable weights to a provider's model id does
// not change — so the horizon bounds exactly one decision: which revision an
// unpinned reference resolves to, which is Oxy's to make and goes stale.
// Refusing that after an hour degrades a fraction of traffic; refusing it after
// a minute would turn every control-plane hiccup into an outage.
const DefaultMaxSnapshotAge = time.Hour

// Deployment is one declared row of the inventory file.
type Deployment struct {
	DeploymentID contract.DeploymentID `json:"deploymentId"`
	Provider     contract.ProviderSlug `json:"provider"`
	// ModelReference is always revision-pinned: a deployment serves specific
	// weights.
	ModelReference contract.ModelReference `json:"modelReference"`
	// UpstreamModelID is what the provider's own API calls this model.
	UpstreamModelID string `json:"upstreamModelId"`
	// Regions are the attested upstream execution/residency regions this
	// deployment may serve from. They are not the AWS region Kaana itself runs
	// in. Absent and empty both mean no regional attestation. Such a deployment
	// may match only an explicitly empty signed region set, which Oxy emits only
	// when the customer's effective policy has no regional control.
	Regions []contract.Region `json:"regions,omitempty"`
	// Current marks the revision an UNPINNED reference to this model line
	// resolves to.
	//
	// Choosing the current revision of a model is described in the contract as
	// Oxy's decision, but the envelope carries no resolution and the stream's
	// start event must report a revision-pinned reference. An authorizedRoutes
	// entry is already pinned by Oxy; only an older no-list request reaches this
	// inventory choice.
	Current bool `json:"current"`
}

// Endpoint is one place a model reference can be served.
//
// It deliberately carries NO model reference. The RouteSet it belongs to holds
// the only one, so pairing this endpoint with anything else is not expressible.
// `TestAnEndpointCannotCarryItsOwnModelReference` gates that by inspecting this
// type, because the field is what a future change would add first.
type Endpoint struct {
	DeploymentID    contract.DeploymentID
	Provider        contract.ProviderSlug
	UpstreamModelID string
	Regions         []contract.Region
}

// DeploymentDescriptor is the operator-safe identity Oxy needs to construct
// one exact authorized route. It deliberately omits UpstreamModelID: that name
// is an adapter implementation detail, not part of the identity Oxy signs, and
// exposing it would make the operator surface a copy of provider configuration.
//
// Regions is always an explicit array. An empty array means no upstream region
// is attested; it must never be rewritten to the AWS region Kaana happens to run
// in.
type DeploymentDescriptor struct {
	DeploymentID   contract.DeploymentID   `json:"deploymentId"`
	ModelReference contract.ModelReference `json:"modelReference"`
	Provider       contract.ProviderSlug   `json:"provider"`
	Regions        []contract.Region       `json:"regions"`
}

// RouteSet is every endpoint serving ONE model reference, in the order the
// inventory declared them.
type RouteSet struct {
	reference contract.ModelReference
	endpoints []Endpoint
}

// Reference is the revision-pinned reference every endpoint in this set serves.
func (s RouteSet) Reference() contract.ModelReference { return s.reference }

// Len is how many endpoints serve the reference.
func (s RouteSet) Len() int { return len(s.endpoints) }

// Candidates builds the routes this set can be served by.
//
// This is the ONLY place a route is constructed from an inventory, and it
// stamps the set's single reference onto every one of them. Two candidates
// therefore differ in where the request is sent and in nothing else.
func (s RouteSet) Candidates() []provider.Route {
	routes := make([]provider.Route, 0, len(s.endpoints))
	for _, endpoint := range s.endpoints {
		routes = append(routes, provider.Route{
			DeploymentID:    endpoint.DeploymentID,
			Provider:        endpoint.Provider,
			ModelReference:  s.reference,
			UpstreamModelID: endpoint.UpstreamModelID,
			Regions:         append([]contract.Region(nil), endpoint.Regions...),
		})
	}
	return routes
}

// Inventory resolves a model reference to the set of routes that serve it.
type Inventory struct {
	snapshotID  string
	issuedAt    time.Time
	maxAge      time.Duration
	byReference map[contract.ModelReference]RouteSet
	currentOf   map[contract.ModelID]RouteSet
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

// ErrSnapshotTooStale is returned when an UNPINNED reference is resolved
// against a snapshot the control plane has not re-issued within the horizon.
//
// It is deliberately not ErrNoRoute: the model exists and its deployments are
// almost certainly still serving. What Kaana has lost is any basis for
// believing that the revision this snapshot calls current is the revision Oxy
// would choose now, and answering with the stale one would substitute weights
// the customer did not ask for on a decision nobody made.
type ErrSnapshotTooStale struct {
	Reference  contract.ModelReference
	SnapshotID string
	Age        time.Duration
	MaxAge     time.Duration
}

func (e ErrSnapshotTooStale) Error() string {
	return fmt.Sprintf("inventory: snapshot %s is %s old (horizon %s), so the current revision of %q cannot be resolved from it",
		e.SnapshotID, e.Age.Round(time.Second), e.MaxAge, e.Reference)
}

// SnapshotID names the configuration this inventory was built from.
func (i *Inventory) SnapshotID() string { return i.snapshotID }

// Age is how long ago the snapshot was issued.
//
// It is measured from the moment the control plane stamped the snapshot — NOT
// from when Kaana read the file. A file re-read every thirty seconds is not
// thirty seconds fresh; it is as fresh as whoever last wrote it says it is.
func (i *Inventory) Age(at time.Time) time.Duration { return at.Sub(i.issuedAt) }

// ServesUnpinned reports whether unpinned references still resolve. It is what
// the health surface projects, so an operator sees a degradation that is
// otherwise only visible as a refusal on a customer request.
func (i *Inventory) ServesUnpinned(at time.Time) bool { return i.Age(at) <= i.maxAge }

type file struct {
	// SnapshotID names this configuration. It appears in logs and on the health
	// surface so two processes can be compared without diffing files.
	SnapshotID string `json:"snapshotId"`
	// IssuedAt is when the control plane produced this snapshot.
	IssuedAt    string       `json:"issuedAt"`
	Deployments []Deployment `json:"deployments"`
}

// Load reads an inventory from a JSON file.
func Load(path string, maxAge time.Duration) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("inventory: reading %s: %w", path, err)
	}
	return Parse(raw, maxAge)
}

// Parse builds an inventory, refusing anything a route resolution would later
// have to guess about.
//
// Every refusal here is a failure that would otherwise surface as a served
// request: an unpinned deployment silently serving whatever the provider's
// alias points at today, two "current" revisions resolving differently per
// process start, an empty upstream model id producing a 404 from the provider
// that reads like an outage, a snapshot with no issue time whose staleness
// could only be guessed at.
func Parse(raw []byte, maxAge time.Duration) (*Inventory, error) {
	if maxAge <= 0 {
		// A zero horizon would read as "never stale", which is the one value an
		// operator must not be able to reach by leaving a field empty.
		return nil, fmt.Errorf("inventory: a staleness horizon of %s would make every snapshot valid forever; pass a positive duration", maxAge)
	}
	var parsed file
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	if len(parsed.Deployments) == 0 {
		return nil, fmt.Errorf("inventory: no deployments declared; nothing could be served")
	}
	if parsed.SnapshotID == "" {
		return nil, fmt.Errorf("inventory: the snapshot declares no snapshotId, so two processes cannot be told apart")
	}
	if parsed.IssuedAt == "" {
		return nil, fmt.Errorf("inventory: the snapshot declares no issuedAt, so its staleness could only be guessed at")
	}
	issuedAt, err := contract.Timestamp(parsed.IssuedAt).Time()
	if err != nil {
		return nil, fmt.Errorf("inventory: issuedAt: %w", err)
	}

	inventory := &Inventory{
		snapshotID:  parsed.SnapshotID,
		issuedAt:    issuedAt,
		maxAge:      maxAge,
		byReference: make(map[contract.ModelReference]RouteSet, len(parsed.Deployments)),
		currentOf:   make(map[contract.ModelID]RouteSet),
	}
	seenIDs := make(map[contract.DeploymentID]struct{}, len(parsed.Deployments))
	currentReference := make(map[contract.ModelID]contract.ModelReference)

	for _, deployment := range parsed.Deployments {
		switch {
		case len(deployment.DeploymentID) == 0 || len(deployment.DeploymentID) > 128:
			return nil, fmt.Errorf("inventory: deploymentId must contain between 1 and 128 characters")
		case !deployment.Provider.Valid():
			return nil, fmt.Errorf("inventory: %s has provider %q, which is not a provider slug", deployment.DeploymentID, deployment.Provider)
		case !deployment.ModelReference.Valid():
			return nil, fmt.Errorf("inventory: %s has model reference %q, which is not a model reference", deployment.DeploymentID, deployment.ModelReference)
		case !deployment.ModelReference.Pinned():
			return nil, fmt.Errorf("inventory: %s must pin an immutable revision (<publisher>/<model>@<revision>), got %q", deployment.DeploymentID, deployment.ModelReference)
		case deployment.UpstreamModelID == "":
			return nil, fmt.Errorf("inventory: %s declares no upstream model id, so nothing could be sent to %s", deployment.DeploymentID, deployment.Provider)
		}
		seenRegions := make(map[contract.Region]struct{}, len(deployment.Regions))
		for index, region := range deployment.Regions {
			if !region.Valid() {
				return nil, fmt.Errorf("inventory: %s has regions[%d] %q, which is not a region", deployment.DeploymentID, index, region)
			}
			if _, duplicate := seenRegions[region]; duplicate {
				return nil, fmt.Errorf("inventory: %s declares region %q twice", deployment.DeploymentID, region)
			}
			seenRegions[region] = struct{}{}
		}

		if _, duplicate := seenIDs[deployment.DeploymentID]; duplicate {
			return nil, fmt.Errorf("inventory: two deployments share the id %s", deployment.DeploymentID)
		}
		seenIDs[deployment.DeploymentID] = struct{}{}

		// Several deployments of one reference is the same-model failover shape.
		// Declaration order selects the no-list primary; a signed route list
		// carries its own exact attempt order and health never reorders it.
		set := inventory.byReference[deployment.ModelReference]
		set.reference = deployment.ModelReference
		set.endpoints = append(set.endpoints, Endpoint{
			DeploymentID:    deployment.DeploymentID,
			Provider:        deployment.Provider,
			UpstreamModelID: deployment.UpstreamModelID,
			Regions:         append([]contract.Region(nil), deployment.Regions...),
		})
		inventory.byReference[deployment.ModelReference] = set

		if deployment.Current {
			line := deployment.ModelReference.ModelID()
			if existing, declared := currentReference[line]; declared && existing != deployment.ModelReference {
				// Two REVISIONS both claiming to be current is the ambiguity
				// that matters. Two deployments of the SAME revision both
				// marked current is just the failover set, and resolves the
				// same way either way.
				return nil, fmt.Errorf("inventory: %q and %q are both current for %q", existing, deployment.ModelReference, line)
			}
			currentReference[line] = deployment.ModelReference
		}
	}

	for line, reference := range currentReference {
		inventory.currentOf[line] = inventory.byReference[reference]
	}
	return inventory, nil
}

// Resolve returns the set of routes serving a model reference.
//
// A pinned reference resolves to exactly that revision or to nothing: a request
// that named specific weights is served or refused, never substituted. That is
// also why a pinned reference is served from a snapshot of ANY age — the
// mapping from immutable weights to a provider's model id is itself immutable,
// so nothing about it can have gone stale.
//
// An unpinned reference resolves to the model line's current revision, which is
// the one thing in this file that a control-plane outage can make wrong. Past
// the staleness horizon it is refused rather than guessed.
func (i *Inventory) Resolve(reference contract.ModelReference, at time.Time) (RouteSet, error) {
	if set, found := i.byReference[reference]; found {
		return set, nil
	}
	if reference.Pinned() {
		return RouteSet{}, ErrNoRoute{Reference: reference}
	}
	if !i.ServesUnpinned(at) {
		return RouteSet{}, ErrSnapshotTooStale{
			Reference:  reference,
			SnapshotID: i.snapshotID,
			Age:        i.Age(at),
			MaxAge:     i.maxAge,
		}
	}
	set, found := i.currentOf[reference.ModelID()]
	if !found {
		return RouteSet{}, ErrNoRoute{Reference: reference}
	}
	return set, nil
}

// Deployments lists every endpoint in the inventory, keyed by deployment id and
// sorted, for the health surface and for startup validation.
func (i *Inventory) Deployments() []Endpoint {
	seen := make(map[contract.DeploymentID]Endpoint)
	for _, set := range i.byReference {
		for _, endpoint := range set.endpoints {
			seen[endpoint.DeploymentID] = endpoint
		}
	}
	endpoints := make([]Endpoint, 0, len(seen))
	for _, endpoint := range seen {
		endpoints = append(endpoints, endpoint)
	}
	sort.Slice(endpoints, func(a, b int) bool { return endpoints[a].DeploymentID < endpoints[b].DeploymentID })
	return endpoints
}

// DeploymentDescriptors lists the exact identities declared by this snapshot,
// sorted by deployment id for a stable operator projection.
//
// Unlike Deployments, this does not collapse entries through a map. Load
// refuses duplicate deployment ids, but retaining every entry here lets the
// HTTP lookup independently fail closed if that invariant ever regresses.
func (i *Inventory) DeploymentDescriptors() []DeploymentDescriptor {
	descriptors := make([]DeploymentDescriptor, 0)
	for _, set := range i.byReference {
		for _, endpoint := range set.endpoints {
			regions := make([]contract.Region, len(endpoint.Regions))
			copy(regions, endpoint.Regions)
			descriptors = append(descriptors, DeploymentDescriptor{
				DeploymentID:   endpoint.DeploymentID,
				ModelReference: set.reference,
				Provider:       endpoint.Provider,
				Regions:        regions,
			})
		}
	}
	sort.Slice(descriptors, func(a, b int) bool {
		return descriptors[a].DeploymentID < descriptors[b].DeploymentID
	})
	return descriptors
}

// CatalogueEntry is one model line this snapshot serves, under the name a
// caller can hold onto.
//
// `Model` is the unpinned name (`anthropic/claude-sonnet-4`) and `Reference` is
// the revision it resolves to right now. A consumer that builds a product
// catalogue wants the first — a name that survives a revision bump — while the
// second is what the stream's start event will report, so both are here rather
// than making the caller guess which one it is holding.
type CatalogueEntry struct {
	Model     contract.ModelID        `json:"model"`
	Reference contract.ModelReference `json:"modelReference"`
	// Providers that can serve it, sorted. Which one DOES serve a given request
	// is a routing decision this list does not predict.
	Providers []contract.ProviderSlug `json:"providers"`
}

// Catalogue lists every model line an unpinned reference can name, sorted.
//
// Built from the current-revision index rather than from every reference,
// because those are different questions: `byReference` includes revisions that
// are still routable but are no longer what the model's name means. A catalogue
// of those would offer names whose meaning changes under the caller.
//
// It reports what the snapshot DECLARES, not what is reachable this second: a
// line whose only provider is failing its circuit is still in here. Liveness is
// the health surface's question, and answering it here would make a catalogue
// read flap.
func (i *Inventory) Catalogue() []CatalogueEntry {
	entries := make([]CatalogueEntry, 0, len(i.currentOf))
	for id, set := range i.currentOf {
		seen := make(map[contract.ProviderSlug]struct{}, len(set.endpoints))
		providers := make([]contract.ProviderSlug, 0, len(set.endpoints))
		for _, endpoint := range set.endpoints {
			if _, already := seen[endpoint.Provider]; already {
				continue
			}
			seen[endpoint.Provider] = struct{}{}
			providers = append(providers, endpoint.Provider)
		}
		sort.Slice(providers, func(a, b int) bool { return providers[a] < providers[b] })
		entries = append(entries, CatalogueEntry{Model: id, Reference: set.Reference(), Providers: providers})
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Model < entries[b].Model })
	return entries
}

// PinnedOnlyReferences lists references no unpinned name resolves to, sorted.
//
// A snapshot where every deployment is marked current leaves this empty, which
// is the ordinary case. It is reported anyway because the alternative is a
// catalogue that silently omits routable models: "I found fewer" and "there are
// fewer" look identical to a reader, and this is the difference between them.
func (i *Inventory) PinnedOnlyReferences() []contract.ModelReference {
	current := make(map[contract.ModelReference]struct{}, len(i.currentOf))
	for _, set := range i.currentOf {
		current[set.Reference()] = struct{}{}
	}
	references := make([]contract.ModelReference, 0)
	for reference := range i.byReference {
		if _, isCurrent := current[reference]; isCurrent {
			continue
		}
		references = append(references, reference)
	}
	sort.Slice(references, func(a, b int) bool { return references[a] < references[b] })
	return references
}

// Providers lists the provider slugs the inventory routes to, sorted. The
// server uses it to refuse at startup if a routable provider has no adapter,
// rather than discovering it on a customer request.
func (i *Inventory) Providers() []contract.ProviderSlug {
	seen := make(map[contract.ProviderSlug]struct{})
	for _, endpoint := range i.Deployments() {
		seen[endpoint.Provider] = struct{}{}
	}
	slugs := make([]contract.ProviderSlug, 0, len(seen))
	for slug := range seen {
		slugs = append(slugs, slug)
	}
	sort.Slice(slugs, func(a, b int) bool { return slugs[a] < slugs[b] })
	return slugs
}
