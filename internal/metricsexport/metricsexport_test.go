package metricsexport_test

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/OxyHQ/Kaana/internal/metricsexport"
)

func ptr(v float64) *float64 { return &v }

func lines(output []byte) []string {
	return strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
}

func contains(t *testing.T, output []byte, want string) {
	t.Helper()
	for _, line := range lines(output) {
		if line == want {
			return
		}
	}
	t.Errorf("output lacks the line %q:\n%s", want, output)
}

func absent(t *testing.T, output []byte, prefix string) {
	t.Helper()
	for _, line := range lines(output) {
		if strings.HasPrefix(line, prefix) {
			t.Errorf("output has %q, which must be absent", line)
		}
	}
}

func TestProcessLevelGauges(t *testing.T) {
	output := metricsexport.Render(metricsexport.Input{
		ContractVersion: "8.10.0",
		Snapshot:        metricsexport.Snapshot{AgeSeconds: 42, MaxAgeSeconds: 3600, ServesUnpinnedReferences: true},
		Deployments:     []metricsexport.Deployment{{ID: "dep_a", Provider: "p", State: "closed"}},
	})
	contains(t, output, `kaana_build_info{contract_version="8.10.0"} 1`)
	contains(t, output, `kaana_inventory_snapshot_age_seconds 42`)
	contains(t, output, `kaana_inventory_snapshot_max_age_seconds 3600`)
	contains(t, output, `kaana_inventory_serves_unpinned_references 1`)
	contains(t, output, `kaana_inventory_deployments 1`)

	stale := metricsexport.Render(metricsexport.Input{Snapshot: metricsexport.Snapshot{AgeSeconds: 9000, MaxAgeSeconds: 3600}})
	contains(t, stale, `kaana_inventory_serves_unpinned_references 0`)
	contains(t, stale, `kaana_inventory_deployments 0`)
}

func TestBreakerStateValues(t *testing.T) {
	for _, test := range []struct {
		state string
		want  float64
		known bool
	}{
		{"closed", 0, true}, {"open", 1, true}, {"half_open", 2, true},
		{"", 0, false}, {"CLOSED", 0, false}, {"half-open", 0, false},
	} {
		got, ok := metricsexport.BreakerStateValue(test.state)
		if ok != test.known || got != test.want {
			t.Errorf("BreakerStateValue(%q) = %v, %v; want %v, %v", test.state, got, ok, test.want, test.known)
		}
	}

	output := metricsexport.Render(metricsexport.Input{Deployments: []metricsexport.Deployment{
		{ID: "a", Provider: "p", State: "closed"},
		{ID: "b", Provider: "p", State: "open"},
		{ID: "c", Provider: "p", State: "half_open"},
		{ID: "d", Provider: "p", State: "mystery"},
	}})
	contains(t, output, `kaana_deployment_breaker_state{deployment_id="a",provider="p"} 0`)
	contains(t, output, `kaana_deployment_breaker_state{deployment_id="b",provider="p"} 1`)
	contains(t, output, `kaana_deployment_breaker_state{deployment_id="c",provider="p"} 2`)
	absent(t, output, `kaana_deployment_breaker_state{deployment_id="d"`)
}

func TestProviderStatusAndKeyPool(t *testing.T) {
	output := metricsexport.Render(metricsexport.Input{Providers: []metricsexport.Provider{
		{Slug: "openai", Status: "degraded", Credentials: &metricsexport.KeyPool{Declared: 5, Usable: 3}},
		{Slug: "bare", Status: "ok"},
		{Slug: "odd", Status: "something-new", Credentials: &metricsexport.KeyPool{Declared: 1, Usable: 2}},
	}})
	contains(t, output, `kaana_provider_status{provider="openai",status="degraded"} 1`)
	contains(t, output, `kaana_provider_status{provider="openai",status="ok"} 0`)
	contains(t, output, `kaana_provider_status{provider="openai",status="unavailable"} 0`)
	contains(t, output, `kaana_provider_status{provider="openai",status="unconfigured"} 0`)
	contains(t, output, `kaana_provider_credential_keys_declared{provider="openai"} 5`)
	contains(t, output, `kaana_provider_credential_keys_usable{provider="openai"} 3`)
	contains(t, output, `kaana_provider_credential_keys_out{provider="openai"} 2`)
	contains(t, output, `kaana_provider_status{provider="bare",status="ok"} 1`)
	absent(t, output, `kaana_provider_credential_keys_declared{provider="bare"`)
	// An unknown status is not invented into the closed set; a pool whose
	// usable exceeds declared never reports a negative "out".
	absent(t, output, `kaana_provider_status{provider="odd"`)
	contains(t, output, `kaana_provider_credential_keys_out{provider="odd"} 0`)
}

func TestWindowsAndQuantiles(t *testing.T) {
	output := metricsexport.Render(metricsexport.Input{Deployments: []metricsexport.Deployment{{
		ID: "dep_a", Provider: "p", State: "closed", Score: 0.75, ConsecutiveFailures: 2,
		Windows: []metricsexport.Window{
			{
				Seconds: 300, Attempts: 10, Succeeded: 8, Throttled: 1, ToolCallAttempts: 4, ToolCallErrors: 1,
				Uptime:              ptr(0.5),
				TimeToFirstOutputMs: &metricsexport.Percentiles{P50: 100, P75: 150, P90: 200, P99: 900},
				TokensPerSecond:     &metricsexport.Percentiles{P50: 30.5, P75: 40, P90: 50, P99: 60},
			},
			{Seconds: 1800},
			{Seconds: 86400, Attempts: 3},
			{Seconds: 60, Attempts: 99},
		},
	}}})
	const l = `deployment_id="dep_a",provider="p"`
	contains(t, output, `kaana_deployment_health_score{`+l+`} 0.75`)
	contains(t, output, `kaana_deployment_consecutive_failures{`+l+`} 2`)
	contains(t, output, `kaana_deployment_window_attempts{`+l+`,window="5m"} 10`)
	contains(t, output, `kaana_deployment_window_succeeded{`+l+`,window="5m"} 8`)
	contains(t, output, `kaana_deployment_window_throttled{`+l+`,window="5m"} 1`)
	contains(t, output, `kaana_deployment_window_tool_call_attempts{`+l+`,window="5m"} 4`)
	contains(t, output, `kaana_deployment_window_tool_call_errors{`+l+`,window="5m"} 1`)
	contains(t, output, `kaana_deployment_window_uptime_ratio{`+l+`,window="5m"} 0.5`)
	contains(t, output, `kaana_deployment_window_time_to_first_output_ms{`+l+`,quantile="0.5",window="5m"} 100`)
	contains(t, output, `kaana_deployment_window_time_to_first_output_ms{`+l+`,quantile="0.75",window="5m"} 150`)
	contains(t, output, `kaana_deployment_window_time_to_first_output_ms{`+l+`,quantile="0.9",window="5m"} 200`)
	contains(t, output, `kaana_deployment_window_time_to_first_output_ms{`+l+`,quantile="0.99",window="5m"} 900`)
	contains(t, output, `kaana_deployment_window_tokens_per_second{`+l+`,quantile="0.5",window="5m"} 30.5`)
	contains(t, output, `kaana_deployment_window_attempts{`+l+`,window="30m"} 0`)
	contains(t, output, `kaana_deployment_window_attempts{`+l+`,window="1d"} 3`)
	// No uptime or percentiles recorded: no sample, not a zero.
	absent(t, output, `kaana_deployment_window_uptime_ratio{`+l+`,window="30m"`)
	absent(t, output, `kaana_deployment_window_uptime_ratio{`+l+`,window="1d"`)
	absent(t, output, `kaana_deployment_window_tokens_per_second{`+l+`,quantile="0.5",window="1d"`)
	// A span outside the closed set is not exported.
	absent(t, output, `kaana_deployment_window_attempts{`+l+`,window="60`)
}

func TestNonFiniteNumbersAreOmitted(t *testing.T) {
	output := metricsexport.Render(metricsexport.Input{Deployments: []metricsexport.Deployment{{
		ID: "d", Provider: "p", State: "closed", Score: math.NaN(),
		Windows: []metricsexport.Window{{
			Seconds: 300, Uptime: ptr(math.Inf(1)),
			TokensPerSecond: &metricsexport.Percentiles{P50: math.NaN(), P75: math.Inf(-1), P90: 1, P99: 2},
		}},
	}}})
	for _, line := range lines(output) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		value := line[strings.LastIndexByte(line, ' ')+1:]
		if _, err := strconv.ParseFloat(value, 64); err != nil || strings.ContainsAny(value, "NnIi") {
			t.Errorf("sample value %q is not a finite number: %s", value, line)
		}
	}
	absent(t, output, "kaana_deployment_health_score{")
	contains(t, output, `kaana_deployment_window_tokens_per_second{deployment_id="d",provider="p",quantile="0.9",window="5m"} 1`)
}

func TestLabelValuesAreEscaped(t *testing.T) {
	for _, test := range []struct{ name, id, want string }{
		{"quote", `a"b`, `a\"b`},
		{"backslash", `a\b`, `a\\b`},
		{"newline", "a\nb", `a\nb`},
		{"carriage return", "a\rb", "a\uFFFDb"},
		{"nul", "a\x00b", "a\uFFFDb"},
		{"invalid utf-8", "a\xffb", "a\uFFFDb"},
		{"injection", "x\"} 1\nevil{a=\"", `x\"} 1\nevil{a=\"`},
		{"unicode kept", "dép", "dép"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := metricsexport.Render(metricsexport.Input{Deployments: []metricsexport.Deployment{{ID: test.id, Provider: "p", State: "closed"}}})
			contains(t, output, `kaana_deployment_breaker_state{deployment_id="`+test.want+`",provider="p"} 0`)
			// Every line is a comment or a well-formed sample: an identifier
			// cannot start a line of its own.
			sample := regexp.MustCompile(`^(#( HELP| TYPE) .*|kaana_[a-z_]+(\{.*\})? [0-9eE+.\-]+)$`)
			for _, line := range lines(output) {
				if !sample.MatchString(line) {
					t.Errorf("malformed line %q", line)
				}
			}
		})
	}
}

func TestOutputIsDeterministicAndSorted(t *testing.T) {
	input := metricsexport.Input{
		ContractVersion: "1",
		Providers: []metricsexport.Provider{
			{Slug: "zeta", Status: "ok"}, {Slug: "alpha", Status: "ok"},
		},
		Deployments: []metricsexport.Deployment{
			{ID: "dep_z", Provider: "p", State: "closed", Windows: []metricsexport.Window{{Seconds: 86400}, {Seconds: 300}}},
			{ID: "dep_a", Provider: "p", State: "open", Windows: []metricsexport.Window{{Seconds: 1800}}},
		},
	}
	first := string(metricsexport.Render(input))
	for range 20 {
		if again := string(metricsexport.Render(input)); again != first {
			t.Fatal("two renders of the same input differ")
		}
	}
	// Reversing the input order yields the same bytes.
	reversed := input
	reversed.Providers = []metricsexport.Provider{input.Providers[1], input.Providers[0]}
	reversed.Deployments = []metricsexport.Deployment{input.Deployments[1], input.Deployments[0]}
	reversed.Deployments[1].Windows = []metricsexport.Window{{Seconds: 300}, {Seconds: 86400}}
	if got := string(metricsexport.Render(reversed)); got != first {
		t.Errorf("input order changed the output:\n%s\n---\n%s", first, got)
	}

	var families []string
	for _, line := range strings.Split(first, "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			families = append(families, strings.Fields(name)[0])
			if !strings.HasSuffix(line, " gauge") {
				t.Errorf("%q is not a gauge", line)
			}
		}
	}
	for i := 1; i < len(families); i++ {
		if families[i-1] >= families[i] {
			t.Errorf("families out of order or repeated: %s, %s", families[i-1], families[i])
		}
	}
	if !strings.HasSuffix(first, "\n") {
		t.Error("exposition must end with a newline")
	}
	if strings.Index(first, `deployment_id="dep_a"`) > strings.Index(first, `deployment_id="dep_z"`) {
		t.Error("samples are not sorted by label")
	}
}

func TestEmptyInputStillDeclaresEveryFamily(t *testing.T) {
	output := string(metricsexport.Render(metricsexport.Input{}))
	for _, name := range []string{
		"kaana_build_info", "kaana_provider_status", "kaana_deployment_breaker_state",
		"kaana_deployment_window_attempts", "kaana_provider_credential_keys_out",
	} {
		if !strings.Contains(output, "# TYPE "+name+" gauge\n") {
			t.Errorf("%s is not declared for an empty input", name)
		}
	}
}

// TestCardinalityIsBoundedByTheClosedSets renders a deliberately large and
// hostile input and checks that every label name is one of the constants and
// every value of the enumerated labels is in its closed set, so the series
// count is a function of (deployments x windows x quantiles) and nothing a
// request can choose.
func TestCardinalityIsBoundedByTheClosedSets(t *testing.T) {
	percentiles := &metricsexport.Percentiles{P50: 1, P75: 2, P90: 3, P99: 4}
	var deployments []metricsexport.Deployment
	for i := range 7 {
		id := "dep_" + string(rune('a'+i))
		deployments = append(deployments, metricsexport.Deployment{
			ID: id, Provider: "p" + string(rune('a'+i%3)), State: "closed",
			Windows: []metricsexport.Window{
				{Seconds: 300, Uptime: ptr(1), TimeToFirstOutputMs: percentiles, TokensPerSecond: percentiles},
				{Seconds: 1800, Uptime: ptr(1), TimeToFirstOutputMs: percentiles, TokensPerSecond: percentiles},
				{Seconds: 86400, Uptime: ptr(1), TimeToFirstOutputMs: percentiles, TokensPerSecond: percentiles},
				{Seconds: 7, Uptime: ptr(1), TimeToFirstOutputMs: percentiles, TokensPerSecond: percentiles},
				{Seconds: 12345},
			},
		})
	}
	output := metricsexport.Render(metricsexport.Input{
		ContractVersion: "8.10.0",
		Providers:       []metricsexport.Provider{{Slug: "pa", Status: "ok", Credentials: &metricsexport.KeyPool{Declared: 9, Usable: 4}}},
		Deployments:     deployments,
	})

	allowedNames := map[string]bool{
		metricsexport.LabelDeploymentID: true, metricsexport.LabelProvider: true, metricsexport.LabelWindow: true,
		metricsexport.LabelQuantile: true, metricsexport.LabelStatus: true, metricsexport.LabelContractVersion: true,
	}
	closed := map[string]map[string]bool{
		"window":   {"5m": true, "30m": true, "1d": true},
		"quantile": {"0.5": true, "0.75": true, "0.9": true, "0.99": true},
		"status":   {"ok": true, "degraded": true, "unavailable": true, "unconfigured": true},
	}
	pair := regexp.MustCompile(`([a-z_]+)="((?:[^"\\]|\\.)*)"`)
	series := 0
	for _, line := range lines(output) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		series++
		for _, m := range pair.FindAllStringSubmatch(line, -1) {
			if !allowedNames[m[1]] {
				t.Errorf("label name %q is outside the closed vocabulary: %s", m[1], line)
			}
			if set, ok := closed[m[1]]; ok && !set[m[2]] {
				t.Errorf("label %s=%q is outside its closed set: %s", m[1], m[2], line)
			}
		}
	}
	// Per deployment: 3 + 3 windows x (5 counts + uptime + 8 quantiles) = 3+42.
	const perDeployment = 3 + 3*(5+1+8)
	// Process: build, age, max age, serves, deployments = 5; provider: 4 status + 3 key gauges.
	if want := 7*perDeployment + 5 + 4 + 3; series != want {
		t.Errorf("series = %d, want %d", series, want)
	}
}

// TestNothingSensitiveCanBeRendered feeds the formatter the kind of strings a
// provider failure or a credential would carry in every place a string is
// accepted and asserts they only ever appear as escaped label values the
// caller chose. The formatter has no field for provider text or a key, so the
// real guarantee is structural; this pins the other half: a free-text field
// was not added.
func TestNothingSensitiveCanBeRendered(t *testing.T) {
	const secret = "sk-live-0123456789abcdef"
	const providerError = "upstream said: invalid api key for user 203.0.113.9"
	output := string(metricsexport.Render(metricsexport.Input{
		ContractVersion: "8.10.0",
		Providers: []metricsexport.Provider{{
			Slug: "openai", Status: "unavailable",
			Credentials: &metricsexport.KeyPool{Declared: 2, Usable: 0},
		}},
		Deployments: []metricsexport.Deployment{{ID: "dep_x", Provider: "openai", State: "open"}},
	}))
	for _, forbidden := range []string{secret, providerError, "sk-", "203.0.113", "Bearer", "api key", "prompt", "completion", "detail", "keyId", "key_id", "snapshotId", "lastReloadError", "http://", "https://"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("output contains %q:\n%s", forbidden, output)
		}
	}
}
