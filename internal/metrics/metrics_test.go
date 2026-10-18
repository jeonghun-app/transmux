package metrics

import (
	"strings"
	"testing"
)

// TestDropSeriesRequiresEveryLabel is the cross-center bug: camera_id is not
// unique on its own, so matching a single label deleted the series of a
// different center's camera that was still running and still holding its
// metric handles.
func TestDropSeriesRequiresEveryLabel(t *testing.T) {
	r := NewRegistry()
	a := r.Counter("segments_total", "help",
		Label{Name: "center_id", Value: "c1"}, Label{Name: "camera_id", Value: "cam1"})
	b := r.Counter("segments_total", "help",
		Label{Name: "center_id", Value: "c2"}, Label{Name: "camera_id", Value: "cam1"})
	a.Add(3)
	b.Add(7)

	r.DropSeries(
		Label{Name: "center_id", Value: "c1"},
		Label{Name: "camera_id", Value: "cam1"},
	)

	out := string(r.Render())
	if strings.Contains(out, `center_id="c1"`) {
		t.Errorf("c1/cam1 should have been dropped\n%s", out)
	}
	if !strings.Contains(out, `center_id="c2"`) {
		t.Errorf("c2/cam1 is a different channel and must survive\n%s", out)
	}
	if !strings.Contains(out, "segments_total{camera_id=\"cam1\",center_id=\"c2\"} 7") {
		t.Errorf("surviving series lost its value\n%s", out)
	}
}

func TestDropSeriesWithNoLabelsIsANoop(t *testing.T) {
	r := NewRegistry()
	r.Counter("x_total", "help", Label{Name: "a", Value: "1"}).Inc()
	r.DropSeries()
	if !strings.Contains(string(r.Render()), "x_total") {
		t.Error("DropSeries with no labels must not wipe the registry")
	}
}

func TestCounterAndGaugeSemantics(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "help")
	c.Inc()
	c.Add(2.5)
	if got := c.Value(); got != 3.5 {
		t.Errorf("counter = %v, want 3.5", got)
	}
	g := r.Gauge("g", "help")
	g.Set(5)
	g.Set(1)
	if got := g.Value(); got != 1 {
		t.Errorf("gauge = %v, want 1", got)
	}
}

// TestSameNameAndLabelsReturnsTheSameSeries matters because every worker
// resolves its handles by name and label at construction time.
func TestSameNameAndLabelsReturnsTheSameSeries(t *testing.T) {
	r := NewRegistry()
	first := r.Counter("c_total", "help", Label{Name: "id", Value: "a"})
	second := r.Counter("c_total", "help", Label{Name: "id", Value: "a"})
	if first != second {
		t.Fatal("the same name and labels must resolve to one series")
	}
	first.Inc()
	second.Inc()
	if got := first.Value(); got != 2 {
		t.Errorf("value = %v, want 2", got)
	}
}

func TestRenderIsPrometheusTextFormat(t *testing.T) {
	r := NewRegistry()
	r.Gauge("transmux_up", "Whether the shard is up.",
		Label{Name: "shard_id", Value: "s0"}).Set(1)
	out := string(r.Render())
	for _, want := range []string{
		"# HELP transmux_up Whether the shard is up.",
		"# TYPE transmux_up gauge",
		`transmux_up{shard_id="s0"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// An empty family must not emit a bare HELP/TYPE pair with no samples, which
// some scrapers treat as a parse error.
func TestRenderSkipsEmptyFamilies(t *testing.T) {
	r := NewRegistry()
	r.Counter("gone_total", "help", Label{Name: "id", Value: "a"})
	r.DropSeries(Label{Name: "id", Value: "a"})
	if out := string(r.Render()); strings.Contains(out, "gone_total") {
		t.Errorf("empty family should not be rendered\n%s", out)
	}
}

func TestRenderUsesPrometheusEscapes(t *testing.T) {
	r := NewRegistry()
	r.Gauge("test", "help\nwith\\slash", Label{Name: "id", Value: "a\tb\n\"c\\"}).Set(1)
	out := string(r.Render())
	if !strings.Contains(out, "# HELP test help\\nwith\\\\slash\n") {
		t.Errorf("invalid HELP escaping: %q", out)
	}
	if !strings.Contains(out, "id=\"a\tb\\n\\\"c\\\\\"") {
		t.Errorf("invalid label escaping: %q", out)
	}
	if strings.Contains(out, `\t`) {
		t.Error("Go-only escape would reject the entire Prometheus scrape")
	}
}

// TestHistogramRendersCumulativeBuckets pins the exposition shape Prometheus
// needs for histogram_quantile: cumulative le buckets ending in +Inf, which
// must equal _count, plus _sum.
func TestHistogramRendersCumulativeBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("req_seconds", "Latency.", []float64{0.1, 1},
		Label{Name: "route", Value: "GET /x"})
	for _, v := range []float64{0.05, 0.1, 0.5, 3} {
		h.Observe(v)
	}
	out := string(r.Render())
	for _, want := range []string{
		"# TYPE req_seconds histogram\n",
		`req_seconds_bucket{route="GET /x",le="0.1"} 2` + "\n",
		`req_seconds_bucket{route="GET /x",le="1"} 3` + "\n",
		`req_seconds_bucket{route="GET /x",le="+Inf"} 4` + "\n",
		`req_seconds_sum{route="GET /x"} 3.65` + "\n",
		`req_seconds_count{route="GET /x"} 4` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestHistogramWithoutLabelsAndUnsortedBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("d_seconds", "help", []float64{5, 1})
	h.Observe(2)
	out := string(r.Render())
	for _, want := range []string{
		`d_seconds_bucket{le="1"} 0`,
		`d_seconds_bucket{le="5"} 1`,
		`d_seconds_bucket{le="+Inf"} 1`,
		"d_seconds_count 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// TestCounterFuncReadsAtScrapeTime covers counters owned by another
// component, such as the recording scanner's failure count: the exported
// value must be the owner's, not a copy that can drift from it.
func TestCounterFuncReadsAtScrapeTime(t *testing.T) {
	r := NewRegistry()
	var n float64
	r.CounterFunc("owned_total", "help", func() float64 { return n })
	n = 7
	out := string(r.Render())
	if !strings.Contains(out, "# TYPE owned_total counter\n") || !strings.Contains(out, "owned_total 7\n") {
		t.Errorf("counter func not rendered at scrape time\n%s", out)
	}
}
