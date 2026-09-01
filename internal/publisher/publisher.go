// Package publisher builds Kaana's deployment inventory from what the
// providers themselves report, and re-issues it on a cadence.
//
// # Why this is a re-issuing loop and not a file writer
//
// `inventory.Store` measures a snapshot's staleness from the snapshot's own
// `issuedAt`, never from when the file was read. That is the only measure that
// survives the failure that matters: a publisher that has stopped leaves a
// perfectly readable file behind, and re-reading it every thirty seconds would
// report it fresh forever. The consequence lands here — an unchanged snapshot
// with an old `issuedAt` is indistinguishable, from Kaana, from a control plane
// that has stopped publishing, and is treated as one. So this loop re-stamps
// and rewrites even when not one byte of the routing content has changed, and
// `DefaultInterval` is well inside `inventory.DefaultMaxSnapshotAge`.
//
// # It runs in a different process from the one that serves
//
// Writing the inventory decides which deployment every model reference resolves
// to, which is a far larger authority than serving one request. Keeping it in a
// separate command means the permission to write it belongs to a task role that
// does nothing else, rather than to the role the serving process runs under.
//
// # It holds nothing Oxy owns
//
// No account, application, credential, price or commercial permission — the
// same line `internal/inventory` draws, drawn again here because this is the
// process that would first be tempted to cross it. The one Oxy-owned concept
// that unavoidably appears in the artefact is model IDENTITY, and it is
// confined to `attribution.go`, which says why.
package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

// DefaultInterval is how often the snapshot is re-issued.
//
// It has to be meaningfully SHORTER than `inventory.DefaultMaxSnapshotAge`, not
// merely different from it: at exactly the horizon every cycle would land on
// the boundary and a single missed publish would degrade unpinned resolution.
// At fifteen minutes, four consecutive publishes can fail before Kaana stops
// resolving unpinned references — a real margin, and still cheap for a job
// whose whole output is a few kilobytes.
//
// `TestTheDefaultCadenceLeavesRoomInsideTheHorizon` is what keeps this honest.
const DefaultInterval = 15 * time.Minute

// Config wires a Publisher.
type Config struct {
	// Providers are the upstreams to ask, in the order they were declared.
	// Only providers holding a credential belong here: a snapshot may not name
	// a provider whose key does not exist, because there is no value of
	// KAANA_PROVIDERS that serves it without either refusing its references or
	// pinning a permanent `unconfigured` alarm.
	Providers   []Provider
	Attribution *Attribution
	Store       ObjectStore
	// Interval is the re-issue cadence. Zero uses DefaultInterval.
	Interval time.Duration
	// Client is the HTTP client used to ask providers. Injectable so a test
	// serves the providers' real wire shape from a fake upstream.
	Client *http.Client
	// Now is the clock, injectable so a test can watch `issuedAt` move without
	// sleeping through the cadence.
	Now    func() time.Time
	Logger *slog.Logger
}

// Publisher re-issues the inventory snapshot.
type Publisher struct {
	providersMu sync.RWMutex
	providers   []Provider
	attribution *Attribution
	store       ObjectStore
	interval    time.Duration
	client      *http.Client
	now         func() time.Time
	logger      *slog.Logger
}

// New validates the wiring and refuses anything that would publish silently
// wrong.
func New(config Config) (*Publisher, error) {
	switch {
	case len(config.Providers) == 0:
		return nil, errors.New("publisher: no providers hold a credential, so there is nothing to ask and a snapshot would name nothing")
	case config.Attribution == nil:
		return nil, errors.New("publisher: no attribution table")
	case config.Store == nil:
		return nil, errors.New("publisher: no object store")
	}

	interval := config.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if interval >= inventory.DefaultMaxSnapshotAge {
		// A cadence at or past the horizon publishes snapshots that are already
		// stale, or stale before their replacement lands. Refused rather than
		// clamped: clamping would hide an operator's mistaken belief about how
		// often this runs.
		return nil, fmt.Errorf("publisher: a re-issue cadence of %s is not shorter than the staleness horizon of %s, so every snapshot would age out before its replacement arrived",
			interval, inventory.DefaultMaxSnapshotAge)
	}

	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Publisher{
		providers:   config.Providers,
		attribution: config.Attribution,
		store:       config.Store,
		interval:    interval,
		client:      client,
		now:         now,
		logger:      logger,
	}, nil
}

// Interval is the cadence this publisher re-issues on.
func (p *Publisher) Interval() time.Duration { return p.interval }

// ReplaceProviders atomically installs a new credential generation for the
// same configured provider set. Discovery never observes a half-rotated set.
func (p *Publisher) ReplaceProviders(providers []Provider) error {
	p.providersMu.Lock()
	defer p.providersMu.Unlock()
	if len(providers) != len(p.providers) {
		return errors.New("publisher: credential reload changed the configured provider set")
	}
	for index, candidate := range providers {
		if candidate.Slug != p.providers[index].Slug || candidate.BaseURL != p.providers[index].BaseURL || candidate.Discovery != p.providers[index].Discovery {
			return fmt.Errorf("publisher: credential reload changed provider configuration at position %d", index+1)
		}
		if candidate.APIKey == "" {
			return fmt.Errorf("publisher: credential reload left provider %q without a key", candidate.Slug)
		}
	}
	p.providers = append([]Provider(nil), providers...)
	return nil
}

// Run publishes immediately, then every interval until ctx is cancelled.
//
// A failed cycle is logged and the loop continues. It does not stop, because
// the failures this job hits are overwhelmingly transient — a provider
// throttling a list request, an expired credential, an S3 blip — and a process
// that exited on the first one would turn a recoverable minute into an outage
// an hour later, when the horizon passed with nobody left to re-issue.
func (p *Publisher) Run(ctx context.Context) error {
	if err := p.PublishOnce(ctx); err != nil {
		// The FIRST cycle is logged loudly and still does not stop the loop:
		// the common cause of a first-cycle failure is a provider that is not
		// answering yet, and the next cycle is fifteen minutes away, not a
		// deploy away.
		p.logger.Error("the first inventory publish failed; retrying on the cadence", "error", err, "interval", p.interval)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.PublishOnce(ctx); err != nil {
				p.logger.Error("the inventory snapshot could not be re-issued; the published one is ageing toward the horizon",
					"error", err, "horizon", inventory.DefaultMaxSnapshotAge)
			}
		}
	}
}

// PublishOnce runs one cycle: read the previous snapshot, ask every provider
// what it serves, build, and write.
//
// The previous snapshot is read on EVERY cycle rather than cached in memory
// because it is this job's only state. Caching it would mean a restarted
// process re-dated every reference from an empty memory, which is the silent
// substitution the observation date exists to prevent.
func (p *Publisher) PublishOnce(ctx context.Context) error {
	previousBody, published, err := p.store.Get(ctx)
	if err != nil {
		// A read that FAILED is not a first run. Publishing here would mint
		// today's date for every model line and re-point every reference a
		// customer has pinned, on the strength of a transient S3 error.
		return fmt.Errorf("the previously published snapshot could not be read, and re-dating every reference on a failed read would silently re-point them: %w", err)
	}

	observations := Observations{}
	if published {
		observations, err = ObservationsFrom(previousBody)
		if err != nil {
			return err
		}
	} else {
		p.logger.Info("no snapshot has been published yet; every model line observed in this cycle takes today's date",
			"destination", p.store.Describe())
	}

	p.providersMu.RLock()
	providers := append([]Provider(nil), p.providers...)
	p.providersMu.RUnlock()
	discoveries := make([]Discovery, 0, len(providers))
	for _, target := range providers {
		models, err := Discover(ctx, p.client, target)
		if err != nil {
			// One provider failing must not withdraw the others. Withdrawing
			// them would refuse references that are serving perfectly well,
			// which is a larger outage than the one provider's absence.
			p.logger.Error("a provider could not be asked what it serves; its deployments are absent from this snapshot",
				"provider", target.Slug, "error", err)
			continue
		}
		discoveries = append(discoveries, Discovery{Provider: target, Models: models})
	}
	if len(discoveries) == 0 {
		return errors.New("no provider could be asked what it serves, so this cycle has nothing to publish; the previously published snapshot is left in place")
	}

	built, err := BuildSnapshot(discoveries, p.attribution, observations, p.now())
	if err != nil {
		return err
	}
	for _, dropped := range built.Unattributed {
		p.logger.Warn("a provider serves a model nobody has attributed to a publisher; it is absent from the snapshot and no reference resolves to it",
			"model", dropped)
	}

	if err := p.store.Put(ctx, built.Body); err != nil {
		return err
	}

	// Logged at Info every cycle on purpose: this line is the only evidence
	// outside the file that the publisher is alive, and its absence is the
	// signal an operator needs before the horizon turns it into refusals.
	p.logger.Info("inventory snapshot published",
		"snapshotId", built.SnapshotID,
		"issuedAt", contract.NewTimestamp(p.now()),
		"deployments", built.Deployments,
		"unchanged", published && built.SnapshotID == snapshotIDOf(previousBody),
		"destination", p.store.Describe())
	return nil
}

// snapshotIDOf reads the id out of a published snapshot, for the "did anything
// change" half of the log line. An unreadable one answers empty, which reads as
// changed — the safe direction, since it never claims stability it cannot see.
func snapshotIDOf(body []byte) string {
	var parsed struct {
		SnapshotID string `json:"snapshotId"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return parsed.SnapshotID
}
