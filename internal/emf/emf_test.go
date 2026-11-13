package emf

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) write(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func newPublisher(t *testing.T, maxSeries int) (*Publisher, *collector) {
	t.Helper()
	c := &collector{}
	p := New(Options{Namespace: "Oxy/Test", Service: "kaana", FlushEvery: time.Hour, MaxSeries: maxSeries, Write: c.write,
		Now: func() time.Time { return time.UnixMilli(1_700_000_000_000) }})
	t.Cleanup(p.Stop)
	return p, c
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("not JSON: %s", line)
	}
	return out
}

func TestACounterIsOneLinePerLabelSetInEmbeddedMetricFormat(t *testing.T) {
	p, c := newPublisher(t, 0)
	failed := Metric{Name: "Failed", Unit: "Count", Kind: Sum, DimensionSets: [][]string{{"Operation"}}}
	p.Record(failed, 1, Labels{"Operation": "responses"})
	p.Record(failed, 2, Labels{"Operation": "responses"})
	p.Record(failed, 5, Labels{"Operation": "embeddings"})
	p.Flush()
	if len(c.lines) != 2 {
		t.Fatalf("lines = %d, want one per label set: %v", len(c.lines), c.lines)
	}
	var responses map[string]any
	for _, line := range c.lines {
		if d := decode(t, line); d["Operation"] == "responses" {
			responses = d
		}
	}
	if responses["Failed"] != float64(3) || responses["Service"] != "kaana" {
		t.Errorf("counter = %v", responses)
	}
	aws := responses["_aws"].(map[string]any)
	spec := aws["CloudWatchMetrics"].([]any)[0].(map[string]any)
	if spec["Namespace"] != "Oxy/Test" || aws["Timestamp"] != float64(1_700_000_000_000) {
		t.Errorf("envelope = %v", aws)
	}
	dims := spec["Dimensions"].([]any)
	if len(dims) != 2 || len(dims[0].([]any)) != 1 || len(dims[1].([]any)) != 2 {
		t.Errorf("dimension sets = %v, want [Service] and [Service Operation]", dims)
	}
	p.Flush()
	if len(c.lines) != 2 {
		t.Error("an empty interval wrote a line")
	}
}

func TestADistributionKeepsPercentilesAndABoundedNumberOfValues(t *testing.T) {
	p, c := newPublisher(t, 0)
	latency := Metric{Name: "Latency", Unit: "Milliseconds", Kind: Distribution}
	for i := 1; i <= 5000; i++ {
		p.Record(latency, float64(i)*1.37, nil)
	}
	p.Flush()
	d := decode(t, c.lines[0])["Latency"].(map[string]any)
	values, counts := d["Values"].([]any), d["Counts"].([]any)
	if len(values) > maxDistributionValues || len(values) != len(counts) {
		t.Fatalf("values %d counts %d, EMF allows %d", len(values), len(counts), maxDistributionValues)
	}
	total := 0.0
	for _, count := range counts {
		total += count.(float64)
	}
	if d["Count"] != float64(5000) || total != 5000 || d["Min"] != 1.37 {
		t.Errorf("distribution lost observations: %v total %v", d, total)
	}
}

func TestAGaugeKeepsTheLastValueAndItMayBeNegative(t *testing.T) {
	p, c := newPublisher(t, 0)
	gauge := Metric{Name: "Depth", Unit: "Count", Kind: Gauge}
	p.Record(gauge, 9, nil)
	p.Record(gauge, -2, nil)
	p.Flush()
	if got := decode(t, c.lines[0])["Depth"]; got != float64(-2) {
		t.Errorf("gauge = %v", got)
	}
}

func TestNonsenseIsIgnoredAndANilPublisherIsSafe(t *testing.T) {
	p, c := newPublisher(t, 0)
	counter := Metric{Name: "N", Unit: "Count", Kind: Sum}
	p.Record(counter, math.NaN(), nil)
	p.Record(counter, math.Inf(1), nil)
	p.Record(counter, -1, nil)
	p.Flush()
	if len(c.lines) != 0 {
		t.Errorf("nonsense was recorded: %v", c.lines)
	}
	var none *Publisher
	none.Record(counter, 1, nil)
	none.Flush()
	none.Stop()
}

// Cardinality is the cost control: a label value traffic can mint must not make
// a series per value.
func TestPastTheSeriesBoundNewSeriesFoldIntoOther(t *testing.T) {
	p, c := newPublisher(t, 3)
	counter := Metric{Name: "N", Unit: "Count", Kind: Sum, DimensionSets: [][]string{{"Outcome"}}}
	for i := 0; i < 50; i++ {
		p.Record(counter, 1, Labels{"Outcome": "code" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	p.Flush()
	if len(c.lines) != 4 {
		t.Fatalf("series = %d, want the first 3 plus one folded 'other'", len(c.lines))
	}
	other := 0.0
	for _, line := range c.lines {
		if d := decode(t, line); d["Outcome"] == "other" {
			other = d["N"].(float64)
		}
	}
	if other != 47 {
		t.Errorf("folded series counted %v, want 47", other)
	}
}

func TestLabelsAreCleanedAndAPromotedDimensionIsAlwaysPresent(t *testing.T) {
	p, c := newPublisher(t, 0)
	m := Metric{Name: "N", Unit: "Count", Kind: Sum, DimensionSets: [][]string{{"Operation"}}}
	p.Record(m, 1, Labels{"bad name": "x", "Service": "forged", "_aws": "x", "Free": strings.Repeat("a", 500)})
	p.Flush()
	d := decode(t, c.lines[0])
	if d["Service"] != "kaana" || d["Operation"] != "unknown" {
		t.Errorf("reserved label or missing dimension: %v", d)
	}
	if _, bad := d["bad name"]; bad {
		t.Error("an invalid label name reached the line")
	}
	if len(d["Free"].(string)) != maxLabelValueLength {
		t.Error("a long label value was not bounded")
	}
}

func TestStopFlushesTheLastInterval(t *testing.T) {
	c := &collector{}
	p := New(Options{Namespace: "Oxy/Test", Service: "kaana", FlushEvery: time.Hour, Write: c.write})
	p.Record(Metric{Name: "N", Unit: "Count", Kind: Sum}, 1, nil)
	p.Stop()
	p.Stop()
	if len(c.lines) != 1 {
		t.Errorf("shutdown lost the last interval: %v", c.lines)
	}
}
