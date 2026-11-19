package metrics

import (
	"context"
	"fmt"
)

// Sample is one series' value: its labels (nil for a metric without labels)
// and the value.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Source supplies the series of a counter or gauge whose value lives with
// its owner, in its own store or counter. The registry calls it once at
// every Gather and never caches what it returns.
type Source func(ctx context.Context) ([]Sample, error)

// RegisterSource makes src the supplier of the named counter or gauge. It
// refuses, with an error naming the metric, a name outside the catalogue, a
// histogram, a nil source and a second source for one metric.
func (r *Registry) RegisterSource(name string, src Source) error {
	e, err := lookup(name)
	if err != nil {
		return err
	}
	if e.Type == Histogram {
		return fmt.Errorf("metric %s is a histogram, which takes observations, not a source", name)
	}
	if src == nil {
		return fmt.Errorf("metric %s: the source is nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[e.pos]
	if f.source != nil {
		return fmt.Errorf("metric %s already has a source", name)
	}
	f.source = src
	return nil
}

// sourced is what one source supplied for one scrape: its series, or
// failed when it returned an error or panicked.
type sourced struct {
	series map[string]*Series
	failed bool
}

// collect calls the source of e. A failing or panicking source and each
// sample that does not fit the catalogue are logged with the metric's name;
// the scrape goes on without them.
func (r *Registry) collect(ctx context.Context, e *entry, src Source) (out sourced) {
	defer func() {
		if p := recover(); p != nil {
			r.logger.Error("metrics source panicked", "metric", e.Name, "panic", fmt.Sprint(p))
			out = sourced{failed: true}
		}
	}()
	samples, err := src(ctx)
	if err != nil {
		r.logger.Error("metrics source failed", "metric", e.Name, "error", err)
		return sourced{failed: true}
	}
	out.series = make(map[string]*Series, len(samples))
	for _, s := range samples {
		values, err := e.labelValues(s.Labels)
		if err == nil {
			err = e.checkValue(s.Value)
		}
		if err != nil {
			r.logger.Error("metrics source returned an invalid sample", "metric", e.Name, "error", err)
			continue
		}
		out.series[key(values)] = &Series{LabelValues: values, Value: s.Value}
	}
	return out
}
