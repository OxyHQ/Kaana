// Package emf publishes Kaana's operational metrics as CloudWatch Embedded
// Metric Format lines on stdout, the way every Oxy backend does
// (`@oxy.so/telemetry/metrics`).
//
// WHY EMF ON STDOUT: Kaana runs on ECS with the awslogs driver, and CloudWatch
// Logs extracts metrics from any log event shaped as EMF: no agent, no sidecar,
// no scraper, no IAM beyond the logging the task already has. A registry only a
// scrape can read measures into a void when nothing scrapes it.
//
// WHY AGGREGATED: observations are folded in memory and flushed once per
// interval as ONE line per (metric, label set). A latency is kept as a
// distribution, so CloudWatch still answers SampleCount, p50 and p99.
//
// WHY MOST LABELS ARE NOT DIMENSIONS: every distinct combination of dimension
// values is a billed custom metric. A label is a dimension only when its metric
// definition names it in DimensionSets, and only labels with a small, closed
// value set belong there. Every other label is a plain property of the line:
// free to store, groupable in Logs Insights.
//
// Nothing here may carry a prompt, a completion, a user IP, an account or
// credential id: labels are closed vocabularies chosen by the caller.
package emf

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind says how observations in one interval combine.
type Kind int

const (
	// Sum adds every value (a counter).
	Sum Kind = iota
	// Distribution keeps the values for percentiles (a latency or a size).
	Distribution
	// Gauge keeps the last value.
	Gauge
)

// Metric defines one CloudWatch metric.
type Metric struct {
	Name string
	// Unit is a CloudWatch unit: Count, Milliseconds, Seconds, Bytes, Percent, None.
	Unit string
	Kind Kind
	// DimensionSets are label names promoted to dimensions, each set published
	// in addition to the always-present [Service] set.
	DimensionSets [][]string
}

// Labels are the properties of one observation.
type Labels map[string]string

// Recorder is what instrumented code depends on. The zero value of an
// interface is nil and every user must tolerate it: use Record on a nil
// *Publisher, which does nothing.
type Recorder interface {
	Record(metric Metric, value float64, labels Labels)
}

const (
	maxDistributionValues = 100
	bucketBase            = 1.05
	maxLabelValueLength   = 128
	defaultMaxSeries      = 2000
)

var labelName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// Options configure a Publisher.
type Options struct {
	// Namespace is the CloudWatch namespace, for example "Oxy/Kaana".
	Namespace string
	// Service is the value of the Service dimension every metric carries.
	Service string
	// FlushEvery is the flush period; zero is 60 seconds, CloudWatch's standard resolution.
	FlushEvery time.Duration
	// MaxSeries bounds the distinct (metric, label set) series held per
	// interval. Past it every NEW series folds into one whose labels read
	// "other", so a label value traffic can mint cannot grow memory or log
	// volume without bound. Zero is 2000.
	MaxSeries int
	// Write receives each finished line without a trailing newline. Nil writes
	// one line on stdout.
	Write func(line string)
	// Sample, when set, runs on the flush tick immediately before each interval
	// is written, and records state that is read rather than observed (a
	// gauge). It shares the publisher's one timer, so sampling adds no
	// goroutine and no second period. It is not run at Stop: a sample taken
	// while shutting down describes nothing. A panic in it is contained.
	Sample func(Recorder)
	// Now is the clock, for tests.
	Now func() time.Time
}

type series struct {
	metric  Metric
	labels  map[string]string
	sum     float64
	last    float64
	count   int64
	min     float64
	max     float64
	buckets map[float64]int64
}

// Publisher folds observations and writes them. A nil *Publisher records nothing.
type Publisher struct {
	options Options
	mu      sync.Mutex
	current map[string]*series
	stop    chan struct{}
	done    chan struct{}
}

// New starts a Publisher. Call Stop on shutdown to flush the last interval.
func New(options Options) *Publisher {
	if options.FlushEvery <= 0 {
		options.FlushEvery = 60 * time.Second
	}
	if options.MaxSeries <= 0 {
		options.MaxSeries = defaultMaxSeries
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Write == nil {
		options.Write = func(line string) { _, _ = os.Stdout.WriteString(line + "\n") }
	}
	p := &Publisher{options: options, current: make(map[string]*series), stop: make(chan struct{}), done: make(chan struct{})}
	go p.loop()
	return p
}

func (p *Publisher) loop() {
	defer close(p.done)
	ticker := time.NewTicker(p.options.FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if p.options.Sample != nil {
				p.sample()
			}
			p.Flush()
		case <-p.stop:
			p.Flush()
			return
		}
	}
}

// sample runs the Sample hook. Metrics are best effort: a failing sampler costs
// its own gauges for one interval and must not take the process down.
func (p *Publisher) sample() {
	defer func() { _ = recover() }()
	p.options.Sample(p)
}

// Stop flushes the current interval and stops the timer.
func (p *Publisher) Stop() {
	if p == nil {
		return
	}
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

// Record folds one observation into the current interval. It never blocks on
// I/O, never panics, and ignores a non-finite value (and a negative one unless
// the metric is a gauge).
func (p *Publisher) Record(metric Metric, value float64, labels Labels) {
	if p == nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	if metric.Kind != Gauge && value < 0 {
		return
	}
	clean := cleanLabels(metric, labels)
	p.mu.Lock()
	defer p.mu.Unlock()
	key := seriesKey(metric, clean)
	if _, known := p.current[key]; !known && len(p.current) >= p.options.MaxSeries {
		for name := range clean {
			clean[name] = "other"
		}
		key = seriesKey(metric, clean)
	}
	entry := p.current[key]
	if entry == nil {
		entry = &series{metric: metric, labels: clean, min: value, max: value, buckets: map[float64]int64{}}
		p.current[key] = entry
	}
	entry.sum += value
	entry.last = value
	entry.count++
	entry.min = math.Min(entry.min, value)
	entry.max = math.Max(entry.max, value)
	if metric.Kind == Distribution {
		entry.buckets[bucketOf(value)]++
	}
}

// Flush writes the current interval now and starts a new one.
func (p *Publisher) Flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	finished := p.current
	p.current = make(map[string]*series)
	p.mu.Unlock()
	if len(finished) == 0 {
		return
	}
	keys := make([]string, 0, len(finished))
	for key := range finished {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	timestamp := p.options.Now().UnixMilli()
	for _, key := range keys {
		if line, ok := p.line(finished[key], timestamp); ok {
			p.write(line)
		}
	}
}

// write hands one line to the sink. A failing sink loses that line, never the
// process or the rest of the interval (the TypeScript publisher catches the
// same way).
func (p *Publisher) write(line string) {
	defer func() { _ = recover() }()
	p.options.Write(line)
}

func (p *Publisher) line(entry *series, timestamp int64) (string, bool) {
	dimensions := [][]string{{"Service"}}
	for _, set := range entry.metric.DimensionSets {
		dimensions = append(dimensions, append([]string{"Service"}, set...))
	}
	var value any
	switch entry.metric.Kind {
	case Sum:
		value = entry.sum
	case Gauge:
		value = entry.last
	default:
		values, counts := boundedDistribution(entry.buckets)
		value = map[string]any{"Values": values, "Counts": counts, "Max": entry.max, "Min": entry.min, "Count": entry.count, "Sum": entry.sum}
	}
	body := map[string]any{
		"_aws": map[string]any{
			"Timestamp": timestamp,
			"CloudWatchMetrics": []any{map[string]any{
				"Namespace":  p.options.Namespace,
				"Dimensions": dimensions,
				"Metrics":    []any{map[string]string{"Name": entry.metric.Name, "Unit": entry.metric.Unit}},
			}},
		},
		"Service":         p.options.Service,
		entry.metric.Name: value,
	}
	for name, label := range entry.labels {
		body[name] = label
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func cleanLabels(metric Metric, labels Labels) map[string]string {
	clean := make(map[string]string, len(labels))
	for name, raw := range labels {
		if !labelName.MatchString(name) || name == "_aws" || name == "Service" || name == metric.Name {
			continue
		}
		if len(raw) > maxLabelValueLength {
			raw = raw[:maxLabelValueLength]
		}
		// Truncation can cut a rune; drop the fragment before judging emptiness,
		// so a value that is only a fragment reads "unknown" too.
		raw = strings.ToValidUTF8(raw, "")
		if raw == "" {
			raw = "unknown"
		}
		clean[name] = raw
	}
	// A dimension named by the definition must be on every line, or CloudWatch
	// drops the metric from it.
	for _, set := range metric.DimensionSets {
		for _, name := range set {
			if _, present := clean[name]; !present {
				clean[name] = "unknown"
			}
		}
	}
	return clean
}

func seriesKey(metric Metric, labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(metric.Name)
	for _, name := range names {
		b.WriteByte(0)
		b.WriteString(name + "=" + labels[name])
	}
	return b.String()
}

// bucketOf collapses values within about 5 percent into one representative.
func bucketOf(value float64) float64 {
	if value <= 0 {
		return 0
	}
	exponent := math.Round(math.Log(value) / math.Log(bucketBase))
	rounded, _ := roundSignificant(math.Pow(bucketBase, exponent), 3)
	return rounded
}

func roundSignificant(value float64, digits int) (float64, bool) {
	if value == 0 {
		return 0, true
	}
	scale := math.Pow(10, float64(digits)-math.Ceil(math.Log10(math.Abs(value))))
	return math.Round(value*scale) / scale, true
}

// boundedDistribution merges neighbouring buckets until EMF accepts them (at
// most 100 distinct values on one line).
func boundedDistribution(buckets map[float64]int64) ([]float64, []int64) {
	values := make([]float64, 0, len(buckets))
	for value := range buckets {
		values = append(values, value)
	}
	sort.Float64s(values)
	counts := make([]int64, len(values))
	for i, value := range values {
		counts[i] = buckets[value]
	}
	for len(values) > maxDistributionValues {
		mergedValues := make([]float64, 0, (len(values)+1)/2)
		mergedCounts := make([]int64, 0, (len(values)+1)/2)
		for i := 0; i < len(values); i += 2 {
			if i+1 < len(values) {
				mergedValues = append(mergedValues, values[i+1])
				mergedCounts = append(mergedCounts, counts[i]+counts[i+1])
			} else {
				mergedValues = append(mergedValues, values[i])
				mergedCounts = append(mergedCounts, counts[i])
			}
		}
		values, counts = mergedValues, mergedCounts
	}
	return values, counts
}
