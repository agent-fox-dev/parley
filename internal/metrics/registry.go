package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
)

// entry is a catalogue metric with what checking a report against it needs.
type entry struct {
	Metric
	pos    int
	closed []map[string]bool // per label, the closed set; nil for an open label
	open   bool              // the metric has an open label
	zero   []Series          // one zero series per closed combination, open labels ""
}

// entries is the catalogue indexed once: registries share it and never
// change it.
var entries, byName = index(catalogue)

func index(metrics []Metric) ([]*entry, map[string]*entry) {
	list := make([]*entry, len(metrics))
	names := make(map[string]*entry, len(metrics))
	for i, m := range metrics {
		e := &entry{Metric: m, pos: i, closed: make([]map[string]bool, len(m.Labels))}
		combos := [][]string{{}}
		for j, l := range m.Labels {
			values := l.Values
			if values == nil {
				e.open = true
				values = []string{""}
			} else {
				e.closed[j] = map[string]bool{}
				for _, v := range values {
					e.closed[j][v] = true
				}
			}
			var next [][]string
			for _, c := range combos {
				for _, v := range values {
					next = append(next, append(slices.Clone(c), v))
				}
			}
			combos = next
		}
		for _, c := range combos {
			e.zero = append(e.zero, e.newSeries(c))
		}
		list[i], names[m.Name] = e, e
	}
	return list, names
}

func lookup(name string) (*entry, error) {
	e, ok := byName[name]
	if !ok {
		return nil, fmt.Errorf("metric %s is not in the catalogue", name)
	}
	return e, nil
}

// newSeries is a zero series of e with the given label values.
func (e *entry) newSeries(values []string) Series {
	s := Series{LabelValues: values}
	if e.Type == Histogram {
		s.Buckets = make([]uint64, len(e.Buckets))
	}
	return s
}

// labelValues checks that labels has exactly e's label names, with a value
// of its set for a closed label, and returns the values in label order. A
// nil map is no labels.
func (e *entry) labelValues(labels map[string]string) ([]string, error) {
	if len(labels) != len(e.Labels) {
		return nil, e.labelsError(labels)
	}
	values := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		v, ok := labels[l.Name]
		if !ok {
			return nil, e.labelsError(labels)
		}
		if e.closed[i] != nil && !e.closed[i][v] {
			return nil, fmt.Errorf("metric %s: %q is not a value of the label %s", e.Name, v, l.Name)
		}
		values[i] = v
	}
	return values, nil
}

func (e *entry) labelsError(labels map[string]string) error {
	want := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		want[i] = l.Name
	}
	return fmt.Errorf("metric %s takes the labels %v, not %v", e.Name, want, slices.Sorted(maps.Keys(labels)))
}

// checkValue refuses what would corrupt a series: a counter only grows, by a
// finite amount, and a histogram cannot count a NaN in any bucket.
func (e *entry) checkValue(v float64) error {
	switch {
	case e.Type == Counter && (!(v >= 0) || math.IsInf(v, 1)):
		return fmt.Errorf("metric %s is a counter and cannot grow by %v", e.Name, v)
	case e.Type == Histogram && math.IsNaN(v):
		return fmt.Errorf("metric %s cannot observe NaN", e.Name)
	}
	return nil
}

// check finds name and its label values for a call that fits a metric of
// type want.
func check(name string, want Type, call string, labels map[string]string) (*entry, []string, error) {
	e, err := lookup(name)
	if err != nil {
		return nil, nil, err
	}
	if e.Type != want {
		return nil, nil, fmt.Errorf("metric %s is a %s; %s takes a %s", name, e.Type, call, want)
	}
	values, err := e.labelValues(labels)
	if err != nil {
		return nil, nil, err
	}
	return e, values, nil
}

func key(values []string) string { return strings.Join(values, "\xff") }

// family is the state of one metric in a registry: its reported series and
// its source.
type family struct {
	series map[string]*Series
	source Source
}

// Registry holds the value of every catalogue metric for one hub. A hub
// keeps its own, so that hubs in one process never share series. It is safe
// for concurrent use.
type Registry struct {
	logger   *slog.Logger
	mu       sync.Mutex
	families []*family
}

// Family is one metric as Gather exports it. Its Metric is the catalogue's
// and must not be changed.
type Family struct {
	Metric
	Series []Series
}

// Series is one series of a family: its label values in the order of the
// metric's labels, and its value. A histogram's series has instead, per
// bucket bound, the cumulative number of observations at or below it, the
// sum and the count of its observations; the count is its +Inf bucket.
type Series struct {
	LabelValues []string
	Value       float64
	Buckets     []uint64
	Sum         float64
	Count       uint64
}

// New returns a registry in which every catalogue metric has its zero
// series. Failing sources are logged to logger; nil discards the log.
func New(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	r := &Registry{logger: logger, families: make([]*family, len(entries))}
	for i := range r.families {
		r.families[i] = &family{series: map[string]*Series{}}
	}
	return r
}

// series returns the reported series of e with the label values, made at
// zero if it has none yet. The caller holds r.mu.
func (r *Registry) series(e *entry, values []string) *Series {
	f := r.families[e.pos]
	k := key(values)
	s, ok := f.series[k]
	if !ok {
		n := e.newSeries(values)
		s = &n
		f.series[k] = s
	}
	return s
}

// Add grows the named counter's series by value.
func (r *Registry) Add(name string, labels map[string]string, value float64) error {
	e, values, err := check(name, Counter, "Add", labels)
	if err == nil {
		err = e.checkValue(value)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.series(e, values).Value += value
	return nil
}

// Set sets the named gauge's series to value.
func (r *Registry) Set(name string, labels map[string]string, value float64) error {
	e, values, err := check(name, Gauge, "Set", labels)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.series(e, values).Value = value
	return nil
}

// Observe adds value to the named histogram's series: to every bucket whose
// bound is at least value, to the sum and to the count.
func (r *Registry) Observe(name string, labels map[string]string, value float64) error {
	e, values, err := check(name, Histogram, "Observe", labels)
	if err == nil {
		err = e.checkValue(value)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.series(e, values)
	for i, bound := range e.Buckets {
		if value <= bound {
			s.Buckets[i]++
		}
	}
	s.Sum += value
	s.Count++
	return nil
}

// SetFamily replaces every series of the named gauge by samples, the
// complete set: a series it does not name is no longer exported, while a
// closed label combination keeps its zero series. One sample that does not
// fit refuses the whole call.
func (r *Registry) SetFamily(name string, samples []Sample) error {
	e, err := lookup(name)
	if err != nil {
		return err
	}
	if e.Type != Gauge {
		return fmt.Errorf("metric %s is a %s; SetFamily takes a gauge", name, e.Type)
	}
	series := make(map[string]*Series, len(samples))
	for _, s := range samples {
		values, err := e.labelValues(s.Labels)
		if err != nil {
			return err
		}
		series[key(values)] = &Series{LabelValues: values, Value: s.Value}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.families[e.pos].series = series
	return nil
}

// Gather returns every catalogue metric in catalogue order, its series
// sorted by their label values. It calls each registered source once,
// outside the registry's lock, so that a slow source holds up no report. A
// metric with a closed label has every closed combination, at 0 where
// nothing was reported; a metric with an open label is exported with its
// zero series, open labels "", only while it has no series of its own. A
// source that fails exports its metric's zero series for this call.
func (r *Registry) Gather(ctx context.Context) []Family {
	r.mu.Lock()
	sources := make([]Source, len(r.families))
	for i, f := range r.families {
		sources[i] = f.source
	}
	r.mu.Unlock()

	supplied := make([]*sourced, len(entries))
	for i, src := range sources {
		if src != nil {
			s := r.collect(ctx, entries[i], src)
			supplied[i] = &s
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Family, len(entries))
	for i, e := range entries {
		own := []map[string]*Series{r.families[i].series}
		if s := supplied[i]; s != nil {
			if s.failed {
				own = nil
			} else {
				own = append(own, s.series)
			}
		}
		merged := map[string]Series{}
		if !e.open || !hasSeries(own) {
			for _, z := range e.zero {
				merged[key(z.LabelValues)] = z
			}
		}
		for _, m := range own {
			for k, s := range m {
				merged[k] = *s
			}
		}
		series := make([]Series, 0, len(merged))
		for _, s := range merged {
			s.LabelValues = slices.Clone(s.LabelValues)
			s.Buckets = slices.Clone(s.Buckets)
			series = append(series, s)
		}
		slices.SortFunc(series, func(a, b Series) int { return slices.Compare(a.LabelValues, b.LabelValues) })
		out[i] = Family{Metric: e.Metric, Series: series}
	}
	return out
}

func hasSeries(sets []map[string]*Series) bool {
	for _, m := range sets {
		if len(m) > 0 {
			return true
		}
	}
	return false
}
