package emf

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

// goldenCase is one case of the shared conformance fixture. The fixture is
// vendored from `@oxy.so/telemetry` (packages/telemetry/fixtures/emf-golden.json
// in OxyHQ/oxy), the TypeScript publisher every other Oxy backend uses; this
// port must emit byte-for-byte the same document for the same observations, or
// the dashboards and alarms built on one backend read differently on the other.
// Refresh the file from Oxy when that fixture changes; never edit it here.
type goldenCase struct {
	Name         string `json:"name"`
	Service      string `json:"service"`
	Namespace    string `json:"namespace"`
	Now          int64  `json:"now"`
	MaxSeries    int    `json:"maxSeries"`
	Definition   goldenDefinition
	Observations []goldenObservation `json:"observations"`
	Expected     []map[string]any    `json:"expectedLines"`
}

type goldenDefinition struct {
	Name          string     `json:"name"`
	Unit          string     `json:"unit"`
	Kind          string     `json:"kind"`
	DimensionSets [][]string `json:"dimensionSets"`
}

type goldenObservation struct {
	Labels map[string]string `json:"labels"`
	// Value is a number, or "NaN", "Infinity", "-Infinity" (JSON has no such number).
	Value any `json:"value"`
}

func (c *goldenCase) UnmarshalJSON(data []byte) error {
	type plain goldenCase
	aux := struct {
		*plain
		Definition goldenDefinition `json:"definition"`
	}{plain: (*plain)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.Definition = aux.Definition
	return nil
}

func goldenValue(t *testing.T, raw any) float64 {
	t.Helper()
	switch v := raw.(type) {
	case float64:
		return v
	case string:
		switch v {
		case "NaN":
			return math.NaN()
		case "Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
	}
	t.Fatalf("unsupported golden value %v", raw)
	return 0
}

func goldenKind(t *testing.T, name string) Kind {
	t.Helper()
	switch name {
	case "sum":
		return Sum
	case "distribution":
		return Distribution
	case "gauge":
		return Gauge
	}
	t.Fatalf("unknown golden kind %q", name)
	return Sum
}

// canonical re-encodes a line with sorted keys so two lines compare by content.
func canonical(t *testing.T, line map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestEmitsTheSameLinesAsTheTypeScriptPublisher(t *testing.T) {
	raw, err := os.ReadFile("testdata/emf-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("the golden fixture holds no case")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var got []string
			p := New(Options{
				Namespace: tc.Namespace, Service: tc.Service, FlushEvery: time.Hour, MaxSeries: tc.MaxSeries,
				Write: func(line string) {
					var decoded map[string]any
					if err := json.Unmarshal([]byte(line), &decoded); err != nil {
						t.Fatalf("not JSON: %s", line)
					}
					got = append(got, canonical(t, decoded))
				},
				Now: func() time.Time { return time.UnixMilli(tc.Now) },
			})
			defer p.Stop()
			metric := Metric{Name: tc.Definition.Name, Unit: tc.Definition.Unit, Kind: goldenKind(t, tc.Definition.Kind), DimensionSets: tc.Definition.DimensionSets}
			for _, observation := range tc.Observations {
				p.Record(metric, goldenValue(t, observation.Value), observation.Labels)
			}
			p.Flush()
			var want []string
			for _, line := range tc.Expected {
				want = append(want, canonical(t, line))
			}
			// Line order is the publisher's own (Go sorts by series key, the
			// TypeScript publisher keeps insertion order); content is the contract.
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("lines differ\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

func TestTruncationNeverSplitsACharacter(t *testing.T) {
	// 127 units of ASCII then an emoji (2 units): 129 > 128, so the emoji goes whole.
	value := repeat("a", 127) + "😀"
	if got := truncateUTF16(value, 128); got != repeat("a", 127) {
		t.Errorf("kept %d bytes, want the 127 ASCII only", len(got))
	}
	if got := truncateUTF16(repeat("a", 126)+"😀", 128); got != repeat("a", 126)+"😀" {
		t.Error("an emoji ending exactly on the limit must be kept")
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
