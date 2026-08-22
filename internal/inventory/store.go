package inventory

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// Store holds the inventory snapshot the data plane is currently serving from,
// and is what lets Kaana keep serving while the control plane is unreachable.
//
// # What a snapshot is for
//
// Kaana's configuration arrives as a file the control plane publishes. If that
// pipeline stops — Oxy is down, a deploy is stuck, the file is truncated
// mid-write — the process must not stop serving, and it must not start
// pretending it knows things it no longer knows. Those are two different
// requirements and this type keeps them apart:
//
//   - A reload that fails leaves the last good snapshot in place, whole. A
//     half-parsed inventory is never installed, so there is no state in which
//     some references resolve and others silently disappear.
//   - A snapshot that is not being re-issued stops answering the one question
//     whose answer decays: which revision an unpinned reference resolves to.
//     See Inventory.Resolve.
//
// # The requirement this places on the publisher
//
// Staleness is measured from the snapshot's own `issuedAt`, not from when Kaana
// last read the file. That is the only measure that survives the failure that
// matters: a publisher that has stopped running leaves a perfectly readable
// file on disk, and re-reading it every thirty seconds would report it fresh
// forever. The consequence is that the publisher MUST re-issue the snapshot on
// a cadence shorter than the horizon even when nothing has changed. An
// unchanged snapshot with an old issuedAt is indistinguishable, from here, from
// a control plane that has stopped publishing — and it is treated as one.
type Store struct {
	path   string
	maxAge time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu      sync.RWMutex
	current *Inventory
	// lastReloadSummary is the most recent failed reload, rendered for the
	// signed health surface: it says what kind of failure it was and never
	// where the file lives. The full error, path and all, goes to the operator
	// log at the moment it happens.
	lastReloadSummary string
}

// Config wires a Store.
type Config struct {
	// Path is the inventory file the control plane publishes to.
	Path string
	// MaxSnapshotAge is the staleness horizon. Zero uses DefaultMaxSnapshotAge.
	MaxSnapshotAge time.Duration
	// Now is the clock, injectable so a test can age a snapshot without
	// sleeping through the horizon.
	Now    func() time.Time
	Logger *slog.Logger
}

// NewStore loads the first snapshot, refusing to start without one.
//
// Starting with no inventory at all is not the outage case this type exists
// for: a process that never had a snapshot has nothing to fall back TO, and
// serving nothing while reporting healthy is worse than refusing to boot.
func NewStore(config Config) (*Store, error) {
	if config.Path == "" {
		return nil, fmt.Errorf("inventory: no snapshot path")
	}
	maxAge := config.MaxSnapshotAge
	if maxAge <= 0 {
		maxAge = DefaultMaxSnapshotAge
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	loaded, err := Load(config.Path, maxAge)
	if err != nil {
		return nil, err
	}
	return &Store{
		path:    config.Path,
		maxAge:  maxAge,
		now:     now,
		logger:  logger,
		current: loaded,
	}, nil
}

// Current returns the snapshot being served from.
func (s *Store) Current() *Inventory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Reload re-reads the snapshot file.
//
// A failed reload is reported to the caller AND retained, but it never disturbs
// what is being served: the previous snapshot stays installed in full. This is
// the whole behaviour under a control-plane outage, and it is why the swap
// happens after the parse rather than during it.
func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return s.reloadFailed(fmt.Errorf("inventory: reading %s: %w", s.path, err),
			"the snapshot file could not be read")
	}
	loaded, err := Parse(raw, s.maxAge)
	if err != nil {
		// A parse error names fields and values, never the path, so it is safe
		// to project as it stands — and it is the one an operator most needs.
		return s.reloadFailed(err, contract.SafeErrorText(err.Error()))
	}

	s.mu.Lock()
	previous := s.current
	s.current = loaded
	s.lastReloadSummary = ""
	s.mu.Unlock()

	if previous.snapshotID != loaded.snapshotID || !previous.issuedAt.Equal(loaded.issuedAt) {
		s.logger.Info("inventory snapshot installed",
			"snapshotId", loaded.snapshotID,
			"issuedAt", contract.NewTimestamp(loaded.issuedAt),
			"deployments", len(loaded.Deployments()))
	}
	return nil
}

// reloadFailed records a failed reload without disturbing what is being served.
//
// The summary is what the signed health surface shows and is deliberately
// path-free: a filesystem layout is host detail Oxy cannot act on, while the
// log line beside it carries the whole error for whoever can.
func (s *Store) reloadFailed(err error, summary string) error {
	s.mu.Lock()
	s.lastReloadSummary = summary
	serving := s.current.snapshotID
	s.mu.Unlock()

	s.logger.Warn("the inventory snapshot could not be reloaded; continuing to serve the last good one",
		"servingSnapshotId", serving, "error", err)
	return err
}

// SnapshotStatus is the operator-facing projection of the configuration the
// data plane is serving from. It names no path, no provider credential and no
// upstream endpoint.
type SnapshotStatus struct {
	SnapshotID    string             `json:"snapshotId"`
	IssuedAt      contract.Timestamp `json:"issuedAt"`
	AgeSeconds    int                `json:"ageSeconds"`
	MaxAgeSeconds int                `json:"maxAgeSeconds"`
	// ServesUnpinnedReferences is false once the snapshot has aged past the
	// horizon. Pinned references continue to be served either way, which is the
	// distinction this whole mechanism exists to make.
	ServesUnpinnedReferences bool `json:"servesUnpinnedReferences"`
	// LastReloadError is the most recent failed reload, if the snapshot has
	// stopped advancing. Its text is redacted like any other operator-facing
	// string, because a parse error quotes the file it failed on.
	LastReloadError string `json:"lastReloadError,omitempty"`
}

// Status projects what is being served from.
func (s *Store) Status() SnapshotStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	at := s.now()
	status := SnapshotStatus{
		SnapshotID:               s.current.snapshotID,
		IssuedAt:                 contract.NewTimestamp(s.current.issuedAt),
		AgeSeconds:               int(s.current.Age(at).Seconds()),
		MaxAgeSeconds:            int(s.maxAge.Seconds()),
		ServesUnpinnedReferences: s.current.ServesUnpinned(at),
	}
	status.LastReloadError = s.lastReloadSummary
	return status
}
