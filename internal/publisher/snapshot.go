package publisher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// snapshotFile is the wire shape, and it is deliberately a separate type from
// `inventory.Deployment` rather than a reuse of it.
//
// The reader's struct is what Kaana ACCEPTS; this is what the publisher
// PRODUCES. Sharing one type would mean a field added for the reader silently
// starts being emitted, and a field the publisher stopped writing would still
// parse. They are checked against each other by round-tripping every built
// snapshot through `inventory.Parse` — the real reader — before it is written.
type snapshotFile struct {
	Comment     []string             `json:"$comment,omitempty"`
	SnapshotID  string               `json:"snapshotId"`
	IssuedAt    contract.Timestamp   `json:"issuedAt"`
	Deployments []snapshotDeployment `json:"deployments"`
}

type snapshotDeployment struct {
	DeploymentID    contract.DeploymentID   `json:"deploymentId"`
	Provider        contract.ProviderSlug   `json:"provider"`
	ModelReference  contract.ModelReference `json:"modelReference"`
	UpstreamModelID string                  `json:"upstreamModelId"`
	Current         bool                    `json:"current"`
}

// revisionPrefix labels what the pin actually records.
//
// Cerebras — and every other provider this build has looked at — exposes no
// immutable revision handle: the ids are bare aliases and `created` is 0. A
// reference must still pin, so the pin says exactly what is known: the date
// this publisher first OBSERVED the alias. A bare date would read as a release
// date nobody measured.
const revisionPrefix = "observed-"

// observationDateLayout is the date in a revision label. Dashes, because a
// revision is `[a-zA-Z0-9._-]`.
const observationDateLayout = "2006-01-02"

// Observations is the first date each model line was seen.
//
// This is the publisher's only state, and it is the reason the previous
// snapshot is READ before a new one is written. A revision label recomputed
// from today's date every cycle would re-point every reference a customer has
// pinned, every day — a silent substitution of weights, which is the one thing
// the whole reference design exists to prevent. So a line that has been seen
// keeps the date it was first seen, forever, and only a genuinely new line gets
// today's.
//
// It is keyed by MODEL LINE and not by (provider, model). Two providers serving
// the same weights must produce ONE reference with two endpoints — the failover
// set — and keying per provider would instead mint two revisions of one model
// line, which `inventory.Parse` refuses outright as two `current` revisions.
type Observations map[contract.ModelID]string

// Discovery is what one provider reported, in the order providers were declared.
type Discovery struct {
	Provider Provider
	Models   []DiscoveredModel
}

// BuildResult is a built snapshot and what had to be dropped to build it.
type BuildResult struct {
	// Body is the exact bytes to publish, already validated by the real reader.
	Body []byte
	// SnapshotID identifies the CONTENT, so an unchanged re-issue keeps its id
	// and only `issuedAt` moves. That is what lets an operator tell "the
	// publisher is alive and nothing changed" from "the routing changed".
	SnapshotID string
	// Deployments is how many routes the snapshot declares.
	Deployments int
	// Observations is the carried-forward state, including any line first seen
	// in this build.
	Observations Observations
	// Unattributed names the (provider, upstream id) pairs nobody attributed.
	// They are omitted from the snapshot; the caller warns about them, because
	// an unattributed model is invisible in the output by construction.
	Unattributed []string
}

// BuildSnapshot renders the inventory file from what the providers reported.
//
// Ordering is load-bearing rather than cosmetic. Failover is off by default, so
// a reference resolves to the deployment declared FIRST and no other; a
// reference whose first deployment sits on a provider this process does not
// serve is refused even when a later one would work. The caller only passes
// providers that have a credential, so every deployment here is servable, and
// within a reference the order is the order providers were declared — which
// makes the operator's `KAANA_PROVIDERS` the primary-choosing knob, in one
// place, rather than an emergent property of a map iteration.
func BuildSnapshot(discoveries []Discovery, attribution *Attribution, previous Observations, at time.Time) (BuildResult, error) {
	if len(discoveries) == 0 {
		return BuildResult{}, fmt.Errorf("publisher: no provider reported any models, so a snapshot would declare nothing and Kaana would refuse it")
	}

	observations := make(Observations, len(previous))
	for line, date := range previous {
		observations[line] = date
	}
	today := at.UTC().Format(observationDateLayout)

	var (
		deployments  []snapshotDeployment
		unattributed []string
	)
	// A model line's endpoints must be contiguous only in the sense that the
	// FIRST one declared is servable; they are appended in provider-declaration
	// order, so the first provider declaring a line owns its primary route.
	for _, discovery := range discoveries {
		for _, model := range discovery.Models {
			line, attributed := attribution.ModelLine(discovery.Provider.Slug, model.UpstreamModelID)
			if !attributed {
				unattributed = append(unattributed, string(discovery.Provider.Slug)+"/"+model.UpstreamModelID)
				continue
			}

			observed, seenBefore := observations[line]
			if !seenBefore {
				observed = today
				observations[line] = observed
			}

			reference := contract.ModelReference(string(line) + "@" + revisionPrefix + observed)
			if !reference.Valid() || !reference.Pinned() {
				return BuildResult{}, fmt.Errorf("publisher: %q is not a pinned model reference", reference)
			}

			deployments = append(deployments, snapshotDeployment{
				DeploymentID:    deploymentID(discovery.Provider.Slug, model.UpstreamModelID, observed),
				Provider:        discovery.Provider.Slug,
				ModelReference:  reference,
				UpstreamModelID: model.UpstreamModelID,
				// Every line here has exactly one revision — its observation —
				// so marking it current is the only answer that resolves an
				// unpinned reference at all. Two revisions of one line cannot
				// arise: the observation is keyed by line and carried forward.
				Current: true,
			})
		}
	}

	if len(deployments) == 0 {
		return BuildResult{}, fmt.Errorf("publisher: every discovered model was unattributed (%s), so the snapshot would be empty", strings.Join(unattributed, ", "))
	}

	file := snapshotFile{
		Comment:     snapshotComment(),
		SnapshotID:  contentID(deployments),
		IssuedAt:    contract.NewTimestamp(at),
		Deployments: deployments,
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return BuildResult{}, fmt.Errorf("publisher: rendering the snapshot: %w", err)
	}
	body = append(body, '\n')

	// The gate that matters: the bytes about to be published are parsed by the
	// SAME code Kaana runs, with the same horizon, before they leave this
	// process. A snapshot that would fail Kaana's parse is a snapshot that
	// leaves the data plane serving its last good one while a green publish
	// reports success — the failure mode with no signal anywhere.
	if _, err := inventory.Parse(body, inventory.DefaultMaxSnapshotAge); err != nil {
		return BuildResult{}, fmt.Errorf("publisher: the built snapshot is one Kaana would refuse: %w", err)
	}

	sort.Strings(unattributed)
	return BuildResult{
		Body:         body,
		SnapshotID:   file.SnapshotID,
		Deployments:  len(deployments),
		Observations: observations,
		Unattributed: unattributed,
	}, nil
}

// ObservationsFrom recovers the first-seen dates from a published snapshot.
//
// This is how the state survives a restart without a database: the artefact the
// publisher already has to write is also the record of what it has already
// seen. A revision it cannot parse back is not silently re-dated — the caller
// treats a failed read as a refusal to publish, never as "no history".
func ObservationsFrom(body []byte) (Observations, error) {
	var parsed snapshotFile
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("publisher: the previous snapshot is not readable: %w", err)
	}
	observations := make(Observations, len(parsed.Deployments))
	for _, deployment := range parsed.Deployments {
		reference := deployment.ModelReference
		if !reference.Valid() || !reference.Pinned() {
			return nil, fmt.Errorf("publisher: the previous snapshot carries %q, which is not a pinned reference", reference)
		}
		revision := string(reference[strings.Index(string(reference), "@")+1:])
		if !strings.HasPrefix(revision, revisionPrefix) {
			return nil, fmt.Errorf("publisher: the previous snapshot pins %q to revision %q, which this publisher did not mint; refusing to re-date it", reference, revision)
		}
		date := strings.TrimPrefix(revision, revisionPrefix)
		if _, err := time.Parse(observationDateLayout, date); err != nil {
			return nil, fmt.Errorf("publisher: the previous snapshot's revision %q carries no readable observation date: %w", revision, err)
		}
		line := reference.ModelID()
		if existing, present := observations[line]; present && existing != date {
			return nil, fmt.Errorf("publisher: the previous snapshot observes %q on two different dates (%s, %s)", line, existing, date)
		}
		observations[line] = date
	}
	return observations, nil
}

// deploymentID names a route stably.
//
// Derived from the three facts that identify it rather than generated, so an
// unchanged snapshot re-issues with the same ids: a fresh id per cycle would
// make every route look new to anything correlating on it, including the health
// surface and Oxy's own `internal_route_id`.
func deploymentID(slug contract.ProviderSlug, upstreamModelID, observed string) contract.DeploymentID {
	sanitize := strings.NewReplacer("-", "_", ".", "_", "/", "_", ":", "_")
	return contract.DeploymentID(fmt.Sprintf("dep_%s_%s_%s%s",
		sanitize.Replace(string(slug)),
		sanitize.Replace(upstreamModelID),
		strings.TrimSuffix(revisionPrefix, "-")+"_",
		sanitize.Replace(observed),
	))
}

// contentID identifies WHAT is being served, not when it was issued.
//
// Hashing the routing content means an unchanged re-issue keeps its id while
// `issuedAt` advances, so the two questions an operator asks — "is the
// publisher alive" and "did routing change" — have two different answers
// instead of one that moves every cycle.
func contentID(deployments []snapshotDeployment) string {
	digest := sha256.New()
	for _, deployment := range deployments {
		// A hash.Hash's Write is documented never to return an error, so the
		// result is discarded explicitly rather than checked into a branch that
		// cannot run. The NUL separators matter: without them two different
		// field splits could render to the same bytes and collide.
		_, _ = fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%s\x00%t\n",
			deployment.DeploymentID, deployment.Provider, deployment.ModelReference,
			deployment.UpstreamModelID, deployment.Current)
	}
	return "snap_" + hex.EncodeToString(digest.Sum(nil))[:16]
}

// snapshotComment travels with every snapshot so the file explains itself where
// it is read, not only where it is built.
func snapshotComment() []string {
	return []string{
		"GENERATED by cmd/kaana-publisher. Do not edit: the next re-issue overwrites it.",
		"Every upstream model id here was read from that provider's own /models endpoint with the operator's credential. Nothing is copied from documentation, and a model no provider reported is not here.",
		"PUBLISHER IS WHO RELEASED THE WEIGHTS, NEVER WHO SERVES THEM. `openai/gpt-oss-120b` served BY Cerebras carries provider `cerebras` and a reference that does not name it. The attribution table is the only place that mapping is declared, and an unattributed model is dropped rather than guessed at.",
		"THE REVISION LABEL IS AN OBSERVATION, NOT A RELEASE. These providers expose no immutable revision handle, so the pin records the date this publisher first saw the alias. That date is carried forward from the previous snapshot forever: re-dating it would silently re-point every reference a customer has pinned.",
		"ORDER IS LOAD-BEARING. Failover is off by default, so a reference resolves to the deployment declared FIRST. Deployments appear in the order KAANA_PROVIDERS declares their providers, and only providers holding a credential are declared at all.",
		"IT HOLDS NOTHING OXY OWNS: no account, application, credential, price or commercial permission. Provider credentials resolve from the environment in cmd/kaana and are never here.",
		"STALENESS IS MEASURED FROM `issuedAt`. This file is re-issued on a cadence shorter than KAANA_INVENTORY_MAX_AGE even when nothing has changed, because an unchanged snapshot with an old issuedAt is indistinguishable from a publisher that has stopped.",
	}
}
