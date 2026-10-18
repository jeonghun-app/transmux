// Package metrics implements a minimal Prometheus text-format registry.
//
// A hand-rolled registry is used instead of the official client library to
// keep the dependency surface at the AWS SDK only. The exposition format is
// simple and stable, and the daemons need just counters, gauges and a
// fixed-bucket histogram for request latency.
//
// Label cardinality is intentionally bounded: camera_id is acceptable
// because a shard runs at most a few hundred channels, but RTSP URLs, error
// strings and object keys must never be used as label values.
package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

type kind string

const (
	kindCounter   kind = "counter"
	kindGauge     kind = "gauge"
	kindHistogram kind = "histogram"
)

// Label is one metric dimension.
type Label struct {
	Name  string
	Value string
}

// Metric is a single time series handle. Callers hold onto it so the hot
// path performs an atomic add rather than a map lookup.
//
// A series is either a stored value, updated with Add or Set, or computed at
// scrape time from collect. The second form exists for values that decay with
// wall-clock time: a stored "seconds since the last segment" is only correct
// while something keeps writing it, and goes stale exactly when the channel
// stops working, which is when it matters.
//
// A histogram series keeps one count per bucket rather than cumulative counts,
// so a scrape derives every le bucket and _count from a single read of each
// bucket and they always agree. _sum is read separately and may lag by the
// observations that race the scrape, which rate() tolerates.
type Metric struct {
	labels  []Label
	bits    atomic.Uint64 // float64 bits; the running sum for a histogram
	collect func() float64
	upper   []float64       // histogram upper bounds, ascending
	counts  []atomic.Uint64 // per bucket, plus a final overflow bucket
}

// Add increments a counter.
func (m *Metric) Add(delta float64) {
	for {
		old := m.bits.Load()
		next := floatBits(bitsFloat(old) + delta)
		if m.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Inc increments a counter by one.
func (m *Metric) Inc() { m.Add(1) }

// Set replaces a gauge value.
func (m *Metric) Set(v float64) { m.bits.Store(floatBits(v)) }

// Observe records one histogram sample.
func (m *Metric) Observe(v float64) {
	m.counts[sort.SearchFloat64s(m.upper, v)].Add(1)
	m.Add(v)
}

// Value reads the current value.
func (m *Metric) Value() float64 {
	if m.collect != nil {
		return m.collect()
	}
	return bitsFloat(m.bits.Load())
}

type family struct {
	name   string
	help   string
	kind   kind
	series map[string]*Metric
}

// Registry holds every metric family.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
	order    []string
}

func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

// Counter returns the counter series for the given name and labels,
// creating it if needed.
func (r *Registry) Counter(name, help string, labels ...Label) *Metric {
	return r.metric(kindCounter, name, help, labels, nil, nil)
}

// Gauge returns the gauge series for the given name and labels.
func (r *Registry) Gauge(name, help string, labels ...Label) *Metric {
	return r.metric(kindGauge, name, help, labels, nil, nil)
}

// CounterFunc registers a counter whose value is read from its owner when the
// registry is rendered. Use it when another component already keeps the
// count, so the exported value cannot drift from the one it reports
// elsewhere. The same locking rules as GaugeFunc apply to collect.
func (r *Registry) CounterFunc(name, help string, collect func() float64, labels ...Label) *Metric {
	return r.metric(kindCounter, name, help, labels, collect, nil)
}

// Histogram returns the histogram series for the given name, bucket upper
// bounds and labels. Every series of a family must use the same buckets.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...Label) *Metric {
	return r.metric(kindHistogram, name, help, labels, nil, buckets)
}

// GaugeFunc registers a gauge whose value is computed when the registry is
// rendered. Use it for anything that depends on the current time.
//
// A stored gauge for a decaying value is only correct while something keeps
// writing it, and goes stale exactly when the channel stops working. collect
// is called while the registry read lock is held, so it must not block or
// register new metrics.
func (r *Registry) GaugeFunc(name, help string, collect func() float64, labels ...Label) *Metric {
	return r.metric(kindGauge, name, help, labels, collect, nil)
}

// DropSeries removes every series carrying all of the given labels. Used when
// a channel is removed so its series do not linger forever.
//
// All labels must match, not any one of them: camera_id alone is not unique
// across centers, so matching on it would delete another center's series while
// that channel is still running and holding its metric handles.
func (r *Registry) DropSeries(match ...Label) {
	if len(match) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.families {
		for key, m := range f.series {
			if hasAllLabels(m.labels, match) {
				delete(f.series, key)
			}
		}
	}
}

func hasAllLabels(have, want []Label) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (r *Registry) metric(k kind, name, help string, labels []Label, collect func() float64, buckets []float64) *Metric {
	sorted := make([]Label, len(labels))
	copy(sorted, labels)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	key := seriesKey(sorted)

	r.mu.RLock()
	if f, ok := r.families[name]; ok {
		if m, ok := f.series[key]; ok {
			r.mu.RUnlock()
			return m
		}
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.families[name]
	if !ok {
		f = &family{name: name, help: help, kind: k, series: make(map[string]*Metric)}
		r.families[name] = f
		r.order = append(r.order, name)
		sort.Strings(r.order)
	}
	if m, ok := f.series[key]; ok {
		return m
	}
	// collect and the buckets are assigned under the lock so a concurrent
	// render cannot observe a half-initialised series.
	m := &Metric{labels: sorted, collect: collect}
	if k == kindHistogram {
		m.upper = append([]float64(nil), buckets...)
		sort.Float64s(m.upper)
		m.counts = make([]atomic.Uint64, len(m.upper)+1)
	}
	f.series[key] = m
	return m
}

// WriteTo renders the registry in Prometheus text exposition format.
func (r *Registry) WriteTo(sb *strings.Builder) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range r.order {
		f := r.families[name]
		if len(f.series) == 0 {
			continue
		}
		fmt.Fprintf(sb, "# HELP %s %s\n", f.name, escapeHelp.Replace(f.help))
		fmt.Fprintf(sb, "# TYPE %s %s\n", f.name, f.kind)
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m := f.series[k]
			if f.kind == kindHistogram {
				writeHistogram(sb, f.name, m)
				continue
			}
			writeSample(sb, f.name, m.labels, m.Value())
		}
	}
}

func writeHistogram(sb *strings.Builder, name string, m *Metric) {
	var cumulative uint64
	for i, upper := range m.upper {
		cumulative += m.counts[i].Load()
		writeSample(sb, name+"_bucket", append(m.labels[:len(m.labels):len(m.labels)],
			Label{Name: "le", Value: fmt.Sprintf("%g", upper)}), float64(cumulative))
	}
	cumulative += m.counts[len(m.upper)].Load()
	writeSample(sb, name+"_bucket", append(m.labels[:len(m.labels):len(m.labels)],
		Label{Name: "le", Value: "+Inf"}), float64(cumulative))
	writeSample(sb, name+"_sum", m.labels, bitsFloat(m.bits.Load()))
	writeSample(sb, name+"_count", m.labels, float64(cumulative))
}

func writeSample(sb *strings.Builder, name string, labels []Label, value float64) {
	sb.WriteString(name)
	if len(labels) > 0 {
		sb.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(sb, "%s=\"%s\"", l.Name, escapeLabel.Replace(l.Value))
		}
		sb.WriteByte('}')
	}
	fmt.Fprintf(sb, " %g\n", value)
}

// Prometheus accepts only backslash, quote and newline escapes in labels.
// Go's %q adds escapes such as \t and \xNN that invalidate a scrape.
var escapeLabel = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var escapeHelp = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

// Render returns the exposition payload.
func (r *Registry) Render() []byte {
	var sb strings.Builder
	r.WriteTo(&sb)
	return []byte(sb.String())
}

func seriesKey(labels []Label) string {
	var sb strings.Builder
	for _, l := range labels {
		sb.WriteString(l.Name)
		sb.WriteByte('\x00')
		sb.WriteString(l.Value)
		sb.WriteByte('\x00')
	}
	return sb.String()
}

// floatBits and bitsFloat let a float64 live inside an atomic.Uint64.
func floatBits(f float64) uint64 { return math.Float64bits(f) }
func bitsFloat(b uint64) float64 { return math.Float64frombits(b) }
