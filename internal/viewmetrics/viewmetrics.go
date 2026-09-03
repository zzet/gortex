// Package viewmetrics is the operational counter registry for the view
// lifecycle: checkout families, routed generations, ref views, materialized
// views and the requests that read through them.
//
// It answers four questions a log line alone cannot: which layers served a
// query, why a view is stale, which evidence classified a checkout, and why a
// generation still exists. Logs carry the ids that make one incident legible;
// these counters carry the shape of the whole population.
//
// The registry follows internal/telemetry's discipline — an allow-list of
// series and a fixed vocabulary per label — for a different reason. There the
// allow-list is a privacy backbone; here it is a cardinality one. A metric
// label may never carry a checkout id, a path, a ref name or a generation id:
// the series catalog below declares every label's complete value set, and a
// value outside it collapses to LabelOther instead of minting a new series.
// That is what makes the registry's size a function of this file rather than
// of how many worktrees a user has.
package viewmetrics

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// LabelOther is the bucket a value outside a label's declared vocabulary
// lands in. It exists so an unforeseen value is visible as "something else"
// rather than either silently dropped or minting an unbounded series.
const LabelOther = "other"

// seriesKind separates the three shapes a series can have. A counter only
// rises, a gauge is a level that moves both ways, and a duration accumulates
// a count and a total so a mean can be read off the pair.
type seriesKind int

const (
	kindCounter seriesKind = iota
	kindGauge
	kindDuration
)

// labelSpec is one label of a series: its name, and the complete set of
// values it may take. An empty vocabulary means the label is free-form, which
// no series in this package declares — it exists only so the type cannot be
// read as "vocabulary optional".
type labelSpec struct {
	name   string
	values []string
}

// spec is one declared series.
type spec struct {
	kind   seriesKind
	labels []labelSpec
}

// Registry holds one process's view-lifecycle counters.
//
// The zero value is not usable; New returns a registry, and the package-level
// functions below write to the process-wide one. Every method is safe for
// concurrent use and is a map write under a short mutex — no I/O, no
// allocation beyond the key, and nothing that can fail.
type Registry struct {
	mu        sync.Mutex
	counters  map[string]int64
	gauges    map[string]int64
	durations map[string]DurationStat
}

// DurationStat is one duration series: how many observations landed in it and
// what they summed to. The pair is kept rather than a histogram because every
// duration here is read as "how long does this take on average, and is that
// changing" — a shape a bucketed histogram would cost more to answer.
type DurationStat struct {
	Count   int64         `json:"count"`
	Total   time.Duration `json:"total"`
	Longest time.Duration `json:"longest"`
}

// Snapshot is a point-in-time copy of a registry. Every map is a defensive
// copy and can be read without holding the registry's lock.
type Snapshot struct {
	Counters  map[string]int64
	Gauges    map[string]int64
	Durations map[string]DurationStat
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		counters:  map[string]int64{},
		gauges:    map[string]int64{},
		durations: map[string]DurationStat{},
	}
}

// process is the registry every call site writes to. One per daemon, the way
// runtimeactivity keeps one tracker for the whole process: the subsystems
// being measured span half a dozen packages and a per-package registry would
// have to be threaded through every constructor to be read back as one.
var process = New()

// Default returns the process-wide registry.
func Default() *Registry { return process }

// Count adds one to a counter series.
func Count(name string, labels ...string) { process.Count(name, labels...) }

// Add adds delta to a counter series.
func Add(name string, delta int64, labels ...string) { process.Add(name, delta, labels...) }

// SetGauge replaces a gauge's level. Use it for a level derived from a full
// pass over the population — a sweep that counted every checkout's state —
// where the new reading supersedes the old one.
func SetGauge(name string, value int64, labels ...string) { process.SetGauge(name, value, labels...) }

// AddGauge moves a gauge by delta. Use it for a level maintained by paired
// events — a lease taken and released, a coordinator installed and dropped —
// where no single caller ever knows the whole population.
func AddGauge(name string, delta int64, labels ...string) { process.AddGauge(name, delta, labels...) }

// Observe records one duration observation.
func Observe(name string, d time.Duration, labels ...string) { process.Observe(name, d, labels...) }

// Read returns a snapshot of the process-wide registry.
func Read() Snapshot { return process.Snapshot() }

// Reset clears the process-wide registry. It is the test seam: a test that
// asserts on emission resets first so it reads its own call's counts.
func Reset() { process.Reset() }

// Count adds one to a counter series.
func (r *Registry) Count(name string, labels ...string) { r.Add(name, 1, labels...) }

// Add adds delta to a counter series. A series that is not declared, or a
// label list that does not match the declaration, is dropped: the catalog is
// the only place a series comes into existence.
func (r *Registry) Add(name string, delta int64, labels ...string) {
	key, ok := seriesKey(name, kindCounter, labels)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[key] += delta
}

// SetGauge replaces a gauge's level.
func (r *Registry) SetGauge(name string, value int64, labels ...string) {
	key, ok := seriesKey(name, kindGauge, labels)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[key] = value
}

// AddGauge moves a gauge by delta.
func (r *Registry) AddGauge(name string, delta int64, labels ...string) {
	key, ok := seriesKey(name, kindGauge, labels)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[key] += delta
}

// Observe records one duration observation. A negative duration is dropped:
// it is a clock going backwards, not an observation, and folding it into the
// total would make the mean lie.
func (r *Registry) Observe(name string, d time.Duration, labels ...string) {
	if d < 0 {
		return
	}
	key, ok := seriesKey(name, kindDuration, labels)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stat := r.durations[key]
	stat.Count++
	stat.Total += d
	if d > stat.Longest {
		stat.Longest = d
	}
	r.durations[key] = stat
}

// Snapshot copies the registry.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Snapshot{
		Counters:  make(map[string]int64, len(r.counters)),
		Gauges:    make(map[string]int64, len(r.gauges)),
		Durations: make(map[string]DurationStat, len(r.durations)),
	}
	for key, value := range r.counters {
		out.Counters[key] = value
	}
	for key, value := range r.gauges {
		out.Gauges[key] = value
	}
	for key, value := range r.durations {
		out.Durations[key] = value
	}
	return out
}

// Reset empties the registry.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters = map[string]int64{}
	r.gauges = map[string]int64{}
	r.durations = map[string]DurationStat{}
}

// Flat renders a snapshot as one map for a status payload: counters and
// gauges by series key, and each duration series as a count and a total in
// milliseconds. Zero-valued series are left out, so a daemon that has served
// no view carries no view block noise.
func (s Snapshot) Flat() map[string]int64 {
	out := make(map[string]int64, len(s.Counters)+len(s.Gauges)+2*len(s.Durations))
	for key, value := range s.Counters {
		if value != 0 {
			out[key] = value
		}
	}
	for key, value := range s.Gauges {
		if value != 0 {
			out[key] = value
		}
	}
	for key, stat := range s.Durations {
		if stat.Count == 0 {
			continue
		}
		out[key+"|count"] = stat.Count
		out[key+"|total_ms"] = stat.Total.Milliseconds()
	}
	return out
}

// seriesKey renders the storage key for one sample, and reports whether the
// sample may be recorded at all.
//
// This is the single place the catalog is enforced, so the allow-list and the
// vocabulary clamp cannot diverge between the counter, gauge and duration
// paths.
func seriesKey(name string, kind seriesKind, labels []string) (string, bool) {
	declared, found := catalog[name]
	if !found || declared.kind != kind || len(labels) != len(declared.labels) {
		return "", false
	}
	if len(labels) == 0 {
		return name, true
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, value := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(declared.labels[i].name)
		b.WriteByte('=')
		b.WriteString(clamp(declared.labels[i], value))
	}
	b.WriteByte('}')
	return b.String(), true
}

// clamp maps a label value onto its declared vocabulary. Anything else — an
// id, a path, a value a later version emits — becomes LabelOther, which is
// what keeps the series count bounded by this file.
func clamp(label labelSpec, value string) string {
	for _, allowed := range label.values {
		if value == allowed {
			return value
		}
	}
	return LabelOther
}

// SeriesNames lists every declared series, in a stable order. A caller that
// renders a metric table (a test, a status payload doc) reads it from here so
// the table cannot drift from the catalog.
func SeriesNames() []string {
	out := make([]string, 0, len(catalog))
	for name := range catalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LabelVocabularies returns the declared vocabulary of every label of one
// series, keyed by label name. It is what the cardinality guard test asserts
// over: the vocabularies are the complete set of values a label can hold, so
// proving none of them is id-shaped proves the whole registry is bounded.
func LabelVocabularies(name string) map[string][]string {
	declared, found := catalog[name]
	if !found {
		return nil
	}
	out := make(map[string][]string, len(declared.labels))
	for _, label := range declared.labels {
		values := make([]string, len(label.values))
		copy(values, label.values)
		out[label.name] = values
	}
	return out
}
