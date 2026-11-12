// Package deploymentstats keeps what each deployment has recently done: how
// many attempts succeeded, were throttled or failed, and how fast the ones that
// delivered were. It is the measurement a routing decision, an operator and a
// public endpoint table all need, and none of them could read before: the
// rotation registry holds a breaker state and one blended score, the cost
// recorder holds one row per attempt in PostgreSQL, and neither answers "how has
// this deployment behaved over the last five minutes".
//
// It records counts and durations only, never a prompt, a completion, a key or
// a caller, so a snapshot can be logged or served whole (docs/rules.md,
// "Secrets and customer data"). It is process-local and in memory: a restart
// starts empty, which a reader sees as zero attempts rather than as health.
//
// # What counts
//
// An attempt counts when the deployment could have done something about it. A
// customer's cancellation and a failure every deployment would share (a request
// the provider cannot express, a content filter) say nothing about the
// deployment, so the caller does not record them; this package takes the
// verdict it is given.
//
// # Uptime
//
// A minute is "up" when at least MinMinuteAttempts attempts were made in it and
// at least UpRatio of them succeeded. A minute with fewer attempts is not
// observed rather than up or down: a deployment that is idle is not a healthy
// one, and one failed request is not an outage. Uptime over a window is up
// minutes over observed minutes, absent when no minute was observed.
package deploymentstats

import (
	"slices"
	"sync"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
)

const (
	// MinMinuteAttempts is the fewest attempts that make a minute observable.
	MinMinuteAttempts = 10
	// UpRatio is the success share at or above which an observable minute is up.
	UpRatio = 0.8

	minutesKept = 24 * 60
	// samplesKept bounds the latency and throughput samples one deployment
	// keeps. Percentiles are over the samples still inside the window, so a busy
	// deployment is described by its most recent traffic.
	samplesKept = 512
)

// Windows are the spans a snapshot reports.
var Windows = []time.Duration{5 * time.Minute, 30 * time.Minute, 24 * time.Hour}

// Outcome is the verdict on one attempt.
type Outcome int

const (
	// Succeeded delivered an answer.
	Succeeded Outcome = iota
	// Throttled was refused for rate. It is a failure that says the deployment
	// is at capacity rather than broken, and is counted apart.
	Throttled
	// Failed was a failure another deployment could have survived.
	Failed
)

// Attempt is what one attempt contributes.
type Attempt struct {
	Outcome Outcome
	// TimeToFirstOutput is zero when no output arrived.
	TimeToFirstOutput time.Duration
	// Latency is the whole attempt.
	Latency time.Duration
	// OutputTokens is what the attempt generated, zero when unknown.
	OutputTokens int
}

type minuteBucket struct {
	minute    int64 // unix minute; zero means unused
	attempts  int
	succeeded int
	throttled int
}

type sample struct {
	at         time.Time
	ttft       time.Duration
	tokensPerS float64 // zero when it could not be measured
}

type series struct {
	minutes [minutesKept]minuteBucket
	samples []sample // ring of at most samplesKept, oldest overwritten
	next    int
}

// Registry holds one series per deployment.
type Registry struct {
	now func() time.Time

	mu     sync.Mutex
	series map[contract.DeploymentID]*series
}

// NewRegistry builds a registry. A nil clock takes time.Now.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{now: now, series: make(map[contract.DeploymentID]*series)}
}

// Record adds one attempt. It is safe for concurrent use and a no-op on a nil
// receiver, so a caller that is not measuring needs no branch.
func (r *Registry) Record(id contract.DeploymentID, attempt Attempt) {
	if r == nil {
		return
	}
	at := r.now()
	minute := at.Unix() / 60

	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.series[id]
	if entry == nil {
		entry = &series{}
		r.series[id] = entry
	}
	bucket := &entry.minutes[minute%minutesKept]
	if bucket.minute != minute {
		*bucket = minuteBucket{minute: minute}
	}
	bucket.attempts++
	switch attempt.Outcome {
	case Succeeded:
		bucket.succeeded++
	case Throttled:
		bucket.throttled++
	}

	if attempt.Outcome != Succeeded || attempt.TimeToFirstOutput <= 0 {
		return
	}
	entry.addSample(sample{at: at, ttft: attempt.TimeToFirstOutput, tokensPerS: tokensPerSecond(attempt)})
}

// tokensPerSecond is output over the time spent generating it, so time to first
// output does not dilute it. Zero when it cannot be measured.
func tokensPerSecond(attempt Attempt) float64 {
	generating := attempt.Latency - attempt.TimeToFirstOutput
	if attempt.OutputTokens <= 0 || generating <= 0 {
		return 0
	}
	return float64(attempt.OutputTokens) / generating.Seconds()
}

func (s *series) addSample(item sample) {
	if len(s.samples) < samplesKept {
		s.samples = append(s.samples, item)
		return
	}
	s.samples[s.next] = item
	s.next = (s.next + 1) % samplesKept
}

// Percentiles are the fifty-, seventy-fifth-, ninetieth- and ninety-ninth
// percentile of a measurement.
type Percentiles struct {
	P50 float64 `json:"p50"`
	P75 float64 `json:"p75"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

// Window is one deployment's behaviour over one span.
type Window struct {
	Seconds   int `json:"seconds"`
	Attempts  int `json:"attempts"`
	Succeeded int `json:"succeeded"`
	Throttled int `json:"throttled"`
	// Uptime is up minutes over observed minutes; absent when no minute had
	// MinMinuteAttempts attempts.
	Uptime *float64 `json:"uptime,omitempty"`
	// TimeToFirstOutputMs and TokensPerSecond are percentiles over the samples
	// inside the window; absent when there are none.
	TimeToFirstOutputMs *Percentiles `json:"timeToFirstOutputMs,omitempty"`
	TokensPerSecond     *Percentiles `json:"tokensPerSecond,omitempty"`
}

// Snapshot is one deployment's windows, shortest first.
type Snapshot struct {
	DeploymentID contract.DeploymentID `json:"deploymentId"`
	Windows      []Window              `json:"windows"`
}

// Project reports the named deployments in the order given. A deployment this
// process has not seen is reported with empty windows, so the projection is the
// inventory's shape and not a traffic history.
func (r *Registry) Project(ids []contract.DeploymentID) []Snapshot {
	if r == nil {
		return nil
	}
	at := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Snapshot, 0, len(ids))
	for _, id := range ids {
		snapshot := Snapshot{DeploymentID: id, Windows: make([]Window, 0, len(Windows))}
		entry := r.series[id]
		for _, span := range Windows {
			snapshot.Windows = append(snapshot.Windows, entry.window(at, span))
		}
		out = append(out, snapshot)
	}
	return out
}

// Retain drops series for deployments no longer in the inventory.
func (r *Registry) Retain(known []contract.DeploymentID) {
	if r == nil {
		return
	}
	keep := make(map[contract.DeploymentID]struct{}, len(known))
	for _, id := range known {
		keep[id] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.series {
		if _, ok := keep[id]; !ok {
			delete(r.series, id)
		}
	}
}

func (s *series) window(at time.Time, span time.Duration) Window {
	window := Window{Seconds: int(span / time.Second)}
	if s == nil {
		return window
	}
	nowMinute := at.Unix() / 60
	oldest := nowMinute - int64(span/time.Minute) + 1
	var observed, up int
	for i := range s.minutes {
		bucket := s.minutes[i]
		if bucket.minute == 0 || bucket.minute < oldest || bucket.minute > nowMinute {
			continue
		}
		window.Attempts += bucket.attempts
		window.Succeeded += bucket.succeeded
		window.Throttled += bucket.throttled
		if bucket.attempts >= MinMinuteAttempts {
			observed++
			if float64(bucket.succeeded)/float64(bucket.attempts) >= UpRatio {
				up++
			}
		}
	}
	if observed > 0 {
		uptime := float64(up) / float64(observed)
		window.Uptime = &uptime
	}

	cutoff := at.Add(-span)
	var ttft, rate []float64
	for _, item := range s.samples {
		if item.at.Before(cutoff) {
			continue
		}
		ttft = append(ttft, float64(item.ttft)/float64(time.Millisecond))
		if item.tokensPerS > 0 {
			rate = append(rate, item.tokensPerS)
		}
	}
	window.TimeToFirstOutputMs = percentiles(ttft)
	window.TokensPerSecond = percentiles(rate)
	return window
}

// percentiles uses the nearest-rank method, which needs no interpolation and
// always reports a value that was observed.
func percentiles(values []float64) *Percentiles {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	rank := func(p float64) float64 {
		index := int(p*float64(len(values))+0.999999999) - 1
		return values[min(max(index, 0), len(values)-1)]
	}
	return &Percentiles{P50: rank(0.50), P75: rank(0.75), P90: rank(0.90), P99: rank(0.99)}
}
