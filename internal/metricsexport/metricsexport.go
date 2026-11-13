// Package metricsexport renders Kaana's in-memory operating state as Prometheus
// text exposition (version 0.0.4).
//
// It is a pure formatter. It reads nothing, records nothing and sits on no hot
// path: the caller projects state that already exists (the inventory snapshot,
// the rotation registry, the deployment statistics, the provider key pools)
// into Input, and Render turns that into bytes. Every number is a gauge sampled
// at scrape time; per-window counts are gauges because the window slides.
//
// What can appear in the output is closed by construction. Label names are the
// constants below. Label values come from three kinds of source only: the
// inventory (deployment id and provider slug, both opaque identifiers Oxy
// already receives on every usage report), fixed enumerations in this file
// (window, quantile, status) and the contract version. A prompt, a completion,
// a user address, a credential, a key id or a provider's error text has no
// field in Input, so it cannot reach a label or a value.
//
// Output is deterministic: families sorted by name, samples sorted by label
// string, label values escaped, and a non-finite number is omitted rather than
// written as NaN or Inf.
package metricsexport

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Label names. This is the whole vocabulary.
const (
	LabelDeploymentID    = "deployment_id"
	LabelProvider        = "provider"
	LabelWindow          = "window"
	LabelQuantile        = "quantile"
	LabelStatus          = "status"
	LabelContractVersion = "contract_version"
)

// Breaker states as Input carries them.
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half_open"
)

// providerStatuses are the coarse adapter statuses, in export order.
var providerStatuses = []string{"ok", "degraded", "unavailable", "unconfigured"}

// windowNames maps a window length in seconds to its label value. A window of
// any other length is not exported, which keeps the label set closed even if the
// statistics registry gains a span.
var windowNames = map[int]string{300: "5m", 1800: "30m", 86400: "1d"}

// quantiles are the exported quantile label values, in order.
var quantileLabels = []string{"0.5", "0.75", "0.9", "0.99"}

// BreakerStateValue is the numeric gauge for a breaker state: 0 closed, 1 open,
// 2 half open. An unknown state is not exported.
func BreakerStateValue(state string) (float64, bool) {
	switch state {
	case StateClosed:
		return 0, true
	case StateOpen:
		return 1, true
	case StateHalfOpen:
		return 2, true
	}
	return 0, false
}

// Percentiles are one measurement's quantiles.
type Percentiles struct {
	P50, P75, P90, P99 float64
}

// Window is one deployment's behaviour over one span. Optional measurements are
// nil when the window has none.
type Window struct {
	Seconds             int
	Attempts            int
	Succeeded           int
	Throttled           int
	ToolCallAttempts    int
	ToolCallErrors      int
	Uptime              *float64
	TimeToFirstOutputMs *Percentiles
	TokensPerSecond     *Percentiles
}

// Deployment is one route in the inventory snapshot.
type Deployment struct {
	ID                  string
	Provider            string
	State               string
	Score               float64
	ConsecutiveFailures int
	Windows             []Window
}

// Provider is one adapter's coarse health and, when it has a pool, its key
// counts. Credentials is nil for a provider with no pool.
type Provider struct {
	Slug        string
	Status      string
	Credentials *KeyPool
}

// KeyPool is counts only; no key id, label or position.
type KeyPool struct {
	Declared int
	Usable   int
}

// Snapshot is the inventory snapshot's staleness.
type Snapshot struct {
	AgeSeconds               int
	MaxAgeSeconds            int
	ServesUnpinnedReferences bool
}

// Input is everything Render exports.
type Input struct {
	ContractVersion string
	Snapshot        Snapshot
	Providers       []Provider
	Deployments     []Deployment
}

type sample struct {
	labels string // rendered with braces, or empty
	value  float64
}

type family struct {
	help    string
	samples []sample
}

type document map[string]*family

// declare registers a family, so its HELP and TYPE are present even when no
// sample follows.
func (d document) declare(name, help string) {
	if d[name] == nil {
		d[name] = &family{help: help}
	}
}

// add appends one sample to a declared family. A non-finite value is dropped.
func (d document) add(name string, value float64, labels ...string) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	d[name].samples = append(d[name].samples, sample{labels: renderLabels(labels), value: value})
}

// renderLabels takes alternating name, value and sorts by name.
func renderLabels(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	type pair struct{ name, value string }
	ordered := make([]pair, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		ordered = append(ordered, pair{pairs[i], pairs[i+1]})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range ordered {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(p.value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue escapes backslash, double quote and newline, as the format
// requires, and replaces every other control character and invalid UTF-8 with
// the replacement rune so a hostile identifier cannot break the line structure.
func escapeLabelValue(value string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(value, "�") {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x20 || r == 0x7f:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// Render produces the exposition for input. Every family is a gauge.
func Render(input Input) []byte {
	d := document{}

	d.declare("kaana_build_info", "Always 1; the contract version this process was built against.")
	d.add("kaana_build_info", 1, LabelContractVersion, input.ContractVersion)

	d.declare("kaana_inventory_snapshot_age_seconds", "Seconds since the served inventory snapshot was issued.")
	d.add("kaana_inventory_snapshot_age_seconds", float64(input.Snapshot.AgeSeconds))
	d.declare("kaana_inventory_snapshot_max_age_seconds", "Staleness horizon of the inventory snapshot, in seconds.")
	d.add("kaana_inventory_snapshot_max_age_seconds", float64(input.Snapshot.MaxAgeSeconds))
	d.declare("kaana_inventory_serves_unpinned_references", "1 while the snapshot is inside its staleness horizon, 0 once unpinned references are refused.")
	d.add("kaana_inventory_serves_unpinned_references", boolValue(input.Snapshot.ServesUnpinnedReferences))
	d.declare("kaana_inventory_deployments", "Deployments in the served inventory snapshot.")
	d.add("kaana_inventory_deployments", float64(len(input.Deployments)))

	d.declare("kaana_provider_status", "1 for the provider's current coarse status, 0 for the others.")
	d.declare("kaana_provider_credential_keys_declared", "Provider keys declared in the pool.")
	d.declare("kaana_provider_credential_keys_usable", "Provider keys usable right now.")
	d.declare("kaana_provider_credential_keys_out", "Provider keys currently retired from rotation.")
	for _, p := range input.Providers {
		known := false
		for _, status := range providerStatuses {
			known = known || status == p.Status
		}
		if known {
			for _, status := range providerStatuses {
				d.add("kaana_provider_status", boolValue(status == p.Status), LabelProvider, p.Slug, LabelStatus, status)
			}
		}
		if p.Credentials != nil {
			out := p.Credentials.Declared - p.Credentials.Usable
			if out < 0 {
				out = 0
			}
			d.add("kaana_provider_credential_keys_declared", float64(p.Credentials.Declared), LabelProvider, p.Slug)
			d.add("kaana_provider_credential_keys_usable", float64(p.Credentials.Usable), LabelProvider, p.Slug)
			d.add("kaana_provider_credential_keys_out", float64(out), LabelProvider, p.Slug)
		}
	}

	d.declare("kaana_deployment_breaker_state", "Breaker state: 0 closed, 1 open, 2 half open.")
	d.declare("kaana_deployment_health_score", "Rotation health score, 0 to 1.")
	d.declare("kaana_deployment_consecutive_failures", "Consecutive attributable failures.")
	d.declare("kaana_deployment_window_attempts", "Attempts inside the sliding window.")
	d.declare("kaana_deployment_window_succeeded", "Succeeded attempts inside the sliding window.")
	d.declare("kaana_deployment_window_throttled", "Throttled attempts inside the sliding window.")
	d.declare("kaana_deployment_window_tool_call_attempts", "Served attempts with at least one tool call inside the window.")
	d.declare("kaana_deployment_window_tool_call_errors", "Served attempts with a malformed tool call inside the window.")
	d.declare("kaana_deployment_window_uptime_ratio", "Up minutes over observed minutes inside the window.")
	d.declare("kaana_deployment_window_time_to_first_output_ms", "Time to first output in milliseconds, by quantile.")
	d.declare("kaana_deployment_window_tokens_per_second", "Output tokens per second, by quantile.")
	for _, dep := range input.Deployments {
		base := []string{LabelDeploymentID, dep.ID, LabelProvider, dep.Provider}
		if value, ok := BreakerStateValue(dep.State); ok {
			d.add("kaana_deployment_breaker_state", value, base...)
		}
		d.add("kaana_deployment_health_score", dep.Score, base...)
		d.add("kaana_deployment_consecutive_failures", float64(dep.ConsecutiveFailures), base...)
		for _, w := range dep.Windows {
			name, ok := windowNames[w.Seconds]
			if !ok {
				continue
			}
			labels := append(append([]string{}, base...), LabelWindow, name)
			d.add("kaana_deployment_window_attempts", float64(w.Attempts), labels...)
			d.add("kaana_deployment_window_succeeded", float64(w.Succeeded), labels...)
			d.add("kaana_deployment_window_throttled", float64(w.Throttled), labels...)
			d.add("kaana_deployment_window_tool_call_attempts", float64(w.ToolCallAttempts), labels...)
			d.add("kaana_deployment_window_tool_call_errors", float64(w.ToolCallErrors), labels...)
			if w.Uptime != nil {
				d.add("kaana_deployment_window_uptime_ratio", *w.Uptime, labels...)
			}
			addQuantiles(d, "kaana_deployment_window_time_to_first_output_ms", w.TimeToFirstOutputMs, labels)
			addQuantiles(d, "kaana_deployment_window_tokens_per_second", w.TokensPerSecond, labels)
		}
	}

	return d.encode()
}

func addQuantiles(d document, name string, p *Percentiles, labels []string) {
	if p == nil {
		return
	}
	values := []float64{p.P50, p.P75, p.P90, p.P99}
	for i, label := range quantileLabels {
		d.add(name, values[i], append(append([]string{}, labels...), LabelQuantile, label)...)
	}
}

func (d document) encode() []byte {
	names := make([]string, 0, len(d))
	for name := range d {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		entry := d[name]
		b.WriteString("# HELP " + name + " " + escapeHelp(entry.help) + "\n")
		b.WriteString("# TYPE " + name + " gauge\n")
		sort.SliceStable(entry.samples, func(i, j int) bool { return entry.samples[i].labels < entry.samples[j].labels })
		for _, s := range entry.samples {
			b.WriteString(name + s.labels + " " + strconv.FormatFloat(s.value, 'g', -1, 64) + "\n")
		}
	}
	return []byte(b.String())
}

func escapeHelp(help string) string {
	help = strings.ReplaceAll(help, `\`, `\\`)
	return strings.ReplaceAll(help, "\n", `\n`)
}
