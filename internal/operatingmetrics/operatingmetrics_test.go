package operatingmetrics_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/deploymentstats"
	"github.com/OxyHQ/Kaana/internal/emf"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/operatingmetrics"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/rotation"
)

var clock = time.Date(2026, 11, 13, 12, 0, 30, 0, time.UTC)

// poolRegistrant is an adapter with a key pool. Health makes a network probe in
// production, so the sampler calling it is a failure.
type poolRegistrant struct {
	t    *testing.T
	slug contract.ProviderSlug
	pool *provider.KeyPool
}

func (r poolRegistrant) Provider() contract.ProviderSlug { return r.slug }
func (r poolRegistrant) Health(context.Context) provider.Health {
	r.t.Error("the sampler called Health, which probes the provider over the network")
	return provider.Health{}
}
func (r poolRegistrant) PlatformCredentials() *provider.KeyPool { return r.pool }

type providers []provider.Registrant

func (p providers) All() []provider.Registrant { return p }

type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) write(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *collector) decoded(t *testing.T) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(c.lines))
	for _, line := range c.lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("not JSON: %s", line)
		}
		out = append(out, decoded)
	}
	return out
}

func metricName(line map[string]any) string {
	spec := line["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)
	return spec["Metrics"].([]any)[0].(map[string]any)["Name"].(string)
}

func dimensionNames(line map[string]any) []string {
	spec := line["_aws"].(map[string]any)["CloudWatchMetrics"].([]any)[0].(map[string]any)
	var names []string
	for _, set := range spec["Dimensions"].([]any) {
		for _, name := range set.([]any) {
			names = append(names, name.(string))
		}
	}
	return names
}

func newInventory(t *testing.T, deployments []map[string]any, maxAge time.Duration, now time.Time) *inventory.Store {
	t.Helper()
	document, err := json.Marshal(map[string]any{"snapshotId": "snap_metrics", "issuedAt": contract.NewTimestamp(clock), "deployments": deployments})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := inventory.NewStore(inventory.Config{Path: path, MaxSnapshotAge: maxAge, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func deployment(id, slug string) map[string]any {
	return map[string]any{"deploymentId": id, "provider": slug, "modelReference": "stub/model@2026-05-01", "upstreamModelId": "private-upstream-name", "current": true}
}

type world struct {
	sources operatingmetrics.Sources
	rotate  *rotation.Registry
	stats   *deploymentstats.Registry
}

func newWorld(t *testing.T, deployments []map[string]any, registrants ...provider.Registrant) world {
	t.Helper()
	rotate := rotation.NewRegistry(rotation.DefaultPolicy(), func() time.Time { return clock })
	stats := deploymentstats.NewRegistry(func() time.Time { return clock })
	return world{
		rotate: rotate,
		stats:  stats,
		sources: operatingmetrics.Sources{
			Inventory: newInventory(t, deployments, time.Hour, clock),
			Rotation:  rotate,
			Stats:     stats,
			Providers: providers(registrants),
			Now:       func() time.Time { return clock },
		},
	}
}

func (w world) sample(t *testing.T, maxSeries int) []map[string]any {
	t.Helper()
	sink := &collector{}
	publisher := emf.New(emf.Options{Namespace: "Oxy/Test", Service: "kaana", FlushEvery: time.Hour, MaxSeries: maxSeries,
		Write: sink.write, Now: func() time.Time { return clock }})
	t.Cleanup(publisher.Stop)
	w.sources.Sample(publisher)
	publisher.Flush()
	return sink.decoded(t)
}

func (w world) serve(id string, n int, attempt deploymentstats.Attempt) {
	for range n {
		w.stats.Record(contract.DeploymentID(id), attempt)
	}
}

func (w world) fail(t *testing.T, id string, n int) {
	t.Helper()
	for range n {
		permit, ok := w.rotate.Admit(contract.DeploymentID(id))
		if !ok {
			t.Fatalf("%s was not admitted", id)
		}
		permit.Failed()
	}
}

func served(ttft, latency time.Duration, tokens int) deploymentstats.Attempt {
	return deploymentstats.Attempt{Outcome: deploymentstats.Succeeded, TimeToFirstOutput: ttft, Latency: latency, OutputTokens: tokens}
}

func find(lines []map[string]any, name, deploymentID string) (map[string]any, bool) {
	for _, line := range lines {
		if metricName(line) == name && (deploymentID == "" || line["DeploymentId"] == deploymentID) {
			return line, true
		}
	}
	return nil, false
}

func gauge(t *testing.T, lines []map[string]any, name, deploymentID string) float64 {
	t.Helper()
	line, ok := find(lines, name, deploymentID)
	if !ok {
		t.Fatalf("no %s line for %q", name, deploymentID)
	}
	return line[name].(float64)
}

func pool(t *testing.T, slug contract.ProviderSlug, retired int, keys ...string) *provider.KeyPool {
	t.Helper()
	declarations := make([]provider.KeyDeclaration, 0, len(keys))
	for index, id := range keys {
		declaration := provider.KeyDeclaration{KeyID: id, Secret: "sk-live-" + id}
		if index < retired {
			declaration.State = provider.CredentialRuntimeState{Reason: provider.KeyExhausted, ObservedAt: clock, RetiredUntil: clock.Add(time.Hour)}
		}
		declarations = append(declarations, declaration)
	}
	built, err := provider.NewKeyPool(slug, declarations, provider.KeyPolicy{}, provider.QuotaHeaders{})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func TestTheSamplerEmitsTheExpectedGaugesForAFakeRegistry(t *testing.T) {
	deployments := []map[string]any{
		deployment("dep_busy", "alpha"), deployment("dep_broken", "beta"),
		deployment("dep_thin", "alpha"), deployment("dep_idle", "beta"),
	}
	w := newWorld(t, deployments,
		poolRegistrant{t: t, slug: "alpha", pool: pool(t, "alpha", 1, "key_a1", "key_a2", "key_a3")},
		poolRegistrant{t: t, slug: "beta", pool: pool(t, "beta", 0, "key_b1")},
	)
	// 12 served attempts in one minute make it observable and up.
	w.serve("dep_busy", 12, served(200*time.Millisecond, 1200*time.Millisecond, 100))
	w.stats.Record("dep_busy", deploymentstats.Attempt{Outcome: deploymentstats.Throttled})
	w.fail(t, "dep_broken", 3)
	w.serve("dep_broken", 3, deploymentstats.Attempt{Outcome: deploymentstats.Failed})
	// Three attempts, none with output: the minute is not observable.
	w.serve("dep_thin", 3, deploymentstats.Attempt{Outcome: deploymentstats.Succeeded})

	lines := w.sample(t, 0)

	for _, tc := range []struct {
		name, deployment string
		want             float64
	}{
		{"InventoryDeployments", "", 4},
		{"InventoryStale", "", 0},
		{"InventorySnapshotAgeSeconds", "", 0},
		{"DeploymentBreakerState", "dep_busy", 0},
		{"DeploymentBreakerState", "dep_broken", 1},
		{"DeploymentConsecutiveFailures", "dep_broken", 3},
		{"DeploymentAttempts5m", "dep_busy", 13},
		{"DeploymentAttempts30m", "dep_busy", 13},
		{"DeploymentSucceeded30m", "dep_busy", 12},
		{"DeploymentThrottled5m", "dep_busy", 1},
		{"DeploymentUptime5m", "dep_busy", 1},
		{"DeploymentTimeToFirstOutputP50Ms30m", "dep_busy", 200},
		{"DeploymentTimeToFirstOutputP99Ms5m", "dep_busy", 200},
		{"DeploymentTokensPerSecondP50" + "5m", "dep_busy", 100},
		{"DeploymentAttempts5m", "dep_thin", 3},
	} {
		if got := gauge(t, lines, tc.name, tc.deployment); got != tc.want {
			t.Errorf("%s{%s} = %v, want %v", tc.name, tc.deployment, got, tc.want)
		}
	}
	if score := gauge(t, lines, "DeploymentHealthScore", "dep_broken"); score < 0 || score >= 1 {
		t.Errorf("a failing deployment's score = %v, want below 1", score)
	}

	// Per provider key counts: alpha has 3 declared, 1 retired.
	for _, line := range lines {
		if line["Provider"] != "alpha" {
			continue
		}
		switch metricName(line) {
		case "ProviderKeysDeclared":
			if line["ProviderKeysDeclared"] != float64(3) {
				t.Errorf("declared = %v", line)
			}
		case "ProviderKeysUsable":
			if line["ProviderKeysUsable"] != float64(2) {
				t.Errorf("usable = %v", line)
			}
		case "ProviderKeysOut":
			if line["ProviderKeysOut"] != float64(1) {
				t.Errorf("out = %v", line)
			}
		}
	}

	// Unobserved is omitted, not zero.
	for _, name := range []string{"DeploymentUptime5m", "DeploymentTimeToFirstOutputP50Ms5m", "DeploymentTokensPerSecondP50" + "5m"} {
		if line, ok := find(lines, name, "dep_thin"); ok {
			t.Errorf("%s was emitted for a deployment that never observed it: %v", name, line)
		}
	}
	// An idle deployment (closed, no failures, no attempts) says nothing.
	for _, line := range lines {
		if line["DeploymentId"] == "dep_idle" {
			t.Errorf("an idle deployment emitted %s", metricName(line))
		}
	}
	// The one-day window is not published.
	for _, line := range lines {
		if strings.HasSuffix(metricName(line), "1d") {
			t.Errorf("a one-day metric was emitted: %s", metricName(line))
		}
	}
}

func TestADeploymentIdIsAPropertyAndNeverADimension(t *testing.T) {
	var deployments []map[string]any
	for i := range 40 {
		deployments = append(deployments, deployment(fmt.Sprintf("dep_%03d", i), []string{"alpha", "beta"}[i%2]))
	}
	w := newWorld(t, deployments)
	for i := range 40 {
		w.serve(fmt.Sprintf("dep_%03d", i), 12, served(100*time.Millisecond, time.Second, 50))
	}
	lines := w.sample(t, 0)

	withProperty := 0
	for _, line := range lines {
		if slices.Contains(dimensionNames(line), "DeploymentId") {
			t.Fatalf("DeploymentId is a dimension of %s", metricName(line))
		}
		if _, ok := line["DeploymentId"]; ok {
			withProperty++
		}
	}
	if withProperty == 0 {
		t.Fatal("no line carries DeploymentId as a property")
	}
}

// Billed series are metric names x (1 + providers): deployments add log lines,
// never metrics.
func TestManyDeploymentsAddLinesButNotBilledSeries(t *testing.T) {
	var deployments []map[string]any
	for i := range 300 {
		deployments = append(deployments, deployment(fmt.Sprintf("dep_%03d", i), []string{"alpha", "beta", "gamma"}[i%3]))
	}
	w := newWorld(t, deployments)
	for i := range 300 {
		w.serve(fmt.Sprintf("dep_%03d", i), 12, served(100*time.Millisecond, time.Second, 50))
	}
	lines := w.sample(t, 10_000)

	billed := map[string]bool{}
	for _, line := range lines {
		name := metricName(line)
		billed[name+"|"] = true // the [Service] aggregate
		if provider, ok := line["Provider"].(string); ok {
			billed[name+"|"+provider] = true
		}
	}
	const metricNames = 3 + 2*9 // state gauges + two windows of nine
	const processGauges = 3     // labelless: the [Service] set only
	const providerCount = 3
	if want := metricNames*(1+providerCount) + processGauges; len(billed) != want {
		t.Errorf("billed series = %d, want %d (names x (1 + providers) + process gauges)", len(billed), want)
	}
	if len(lines) < 300 {
		t.Errorf("lines = %d, want one per active deployment per metric", len(lines))
	}
}

// A property with a high-cardinality value set still cannot grow the interval
// without bound: past MaxSeries new series fold into one "other" series each.
func TestPastTheSeriesBoundDeploymentsFoldIntoOther(t *testing.T) {
	var deployments []map[string]any
	for i := range 200 {
		deployments = append(deployments, deployment(fmt.Sprintf("dep_%03d", i), "alpha"))
	}
	w := newWorld(t, deployments)
	for i := range 200 {
		w.serve(fmt.Sprintf("dep_%03d", i), 12, served(100*time.Millisecond, time.Second, 50))
	}
	const bound = 60
	lines := w.sample(t, bound)

	const metricNames = 3 + 2*9
	if len(lines) > bound+metricNames {
		t.Errorf("lines = %d, want at most the bound %d plus one folded series per metric (%d)", len(lines), bound, metricNames)
	}
	folded := 0
	for _, line := range lines {
		if slices.Contains(dimensionNames(line), "DeploymentId") {
			t.Fatal("DeploymentId became a dimension")
		}
		if line["DeploymentId"] == "other" {
			folded++
			if line["Provider"] != "other" {
				t.Errorf("a folded line kept its provider: %v", line)
			}
		}
	}
	if folded == 0 {
		t.Error("nothing folded into other")
	}
}

func TestNothingSecretOrFreeFormReachesALine(t *testing.T) {
	w := newWorld(t, []map[string]any{deployment("dep_busy", "alpha")},
		poolRegistrant{t: t, slug: "alpha", pool: pool(t, "alpha", 1, "key_private_label", "key_two")})
	w.serve("dep_busy", 12, served(100*time.Millisecond, time.Second, 50))
	w.fail(t, "dep_busy", 3)

	sink := &collector{}
	publisher := emf.New(emf.Options{Namespace: "Oxy/Test", Service: "kaana", FlushEvery: time.Hour, Write: sink.write})
	t.Cleanup(publisher.Stop)
	w.sources.Sample(publisher)
	publisher.Flush()
	if len(sink.lines) == 0 {
		t.Fatal("nothing was sampled")
	}
	for _, line := range sink.lines {
		for _, forbidden := range []string{"key_private_label", "key_two", "sk-live", "private-upstream-name", "stub/model", "snap_metrics"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("a line carries %q: %s", forbidden, line)
			}
		}
	}
}

func TestAStaleSnapshotIsReported(t *testing.T) {
	w := newWorld(t, []map[string]any{deployment("dep_a", "alpha")})
	w.sources.Inventory = newInventory(t, []map[string]any{deployment("dep_a", "alpha")}, time.Minute, clock.Add(10*time.Minute))
	lines := w.sample(t, 0)
	if got := gauge(t, lines, "InventoryStale", ""); got != 1 {
		t.Errorf("InventoryStale = %v, want 1 past the horizon", got)
	}
	if got := gauge(t, lines, "InventorySnapshotAgeSeconds", ""); got != 600 {
		t.Errorf("age = %v, want 600", got)
	}
}
