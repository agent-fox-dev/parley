package metrics_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// samples flattens a family into its exposition samples: the key is the
// series' labels as name=value pairs, a histogram's samples suffixed with
// |le=<bound>, |le=+Inf, |sum and |count.
func samples(f metrics.Family) map[string]float64 {
	out := map[string]float64{}
	for _, s := range f.Series {
		pairs := make([]string, len(f.Labels))
		for i, l := range f.Labels {
			pairs[i] = l.Name + "=" + s.LabelValues[i]
		}
		key := strings.Join(pairs, ",")
		if f.Type != metrics.Histogram {
			out[key] = s.Value
			continue
		}
		cumulative := make([]float64, len(s.Buckets))
		for i, n := range s.Buckets {
			cumulative[i] = float64(n)
		}
		maps.Copy(out, histogram(key, f.Buckets, cumulative, float64(s.Count), s.Sum))
	}
	return out
}

// histogram is the samples of one histogram series with the given
// cumulative bucket counts.
func histogram(key string, bounds, cumulative []float64, count, sum float64) map[string]float64 {
	out := map[string]float64{key + "|le=+Inf": count, key + "|sum": sum, key + "|count": count}
	for i, b := range bounds {
		out[key+"|le="+strconv.FormatFloat(b, 'g', -1, 64)] = cumulative[i]
	}
	return out
}

// family returns the named family of a gathered registry.
func family(t *testing.T, families []metrics.Family, name string) metrics.Family {
	t.Helper()
	for _, f := range families {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no family %q", name)
	return metrics.Family{}
}

type report = func(*metrics.Registry, string, map[string]string, float64) error

var (
	add     report = (*metrics.Registry).Add
	set     report = (*metrics.Registry).Set
	observe report = (*metrics.Registry).Observe
)

type call struct {
	do     report
	name   string
	labels map[string]string
	value  float64
}

func TestRegistry_AppliesReports(t *testing.T) {
	ws1Tokens := map[string]string{"workspace": "ws1", "model": "m", "direction": "input"}
	getX := map[string]string{"method": "GET", "path": "/x"}
	tests := []struct {
		name   string
		calls  []call
		family string
		want   map[string]float64
	}{
		{"a counter grows by the amount", []call{
			{add, "parley_lands_total", map[string]string{"result": "landed"}, 1},
			{add, "parley_lands_total", map[string]string{"result": "landed"}, 1},
		}, "parley_lands_total", map[string]float64{
			"result=landed": 2, "result=conflict": 0, "result=blocked": 0, "result=failed": 0}},
		{"a reported series replaces the open-label placeholder", []call{
			{add, "parley_agent_tokens_total", ws1Tokens, 120},
		}, "parley_agent_tokens_total", map[string]float64{"workspace=ws1,model=m,direction=input": 120}},
		{"a gauge takes the last value, nil labels for none", []call{
			{set, "parley_events_seq", nil, 41},
			{set, "parley_events_seq", map[string]string{}, 42},
		}, "parley_events_seq", map[string]float64{"": 42}},
		{"a gauge with an open label", []call{
			{set, "parley_changes_open", map[string]string{"workspace": "ws1"}, 3},
		}, "parley_changes_open", map[string]float64{"workspace=ws1": 3}},
		{"a histogram counts in every bucket at or above the value", []call{
			{observe, "parley_land_attempts", nil, 2},
			{observe, "parley_land_attempts", nil, 21},
		}, "parley_land_attempts", histogram("", attemptBuckets, []float64{0, 1, 1, 1, 1, 1}, 2, 23)},
		{"a value equal to a bound falls in that bucket", []call{
			{observe, "parley_http_request_duration_seconds", getX, 0.005},
		}, "parley_http_request_duration_seconds", histogram("method=GET,path=/x", httpBuckets,
			[]float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, 1, 0.005)},
	}
	ctx := context.Background()
	fresh := metrics.New(nil).Gather(ctx)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := metrics.New(nil)
			for _, c := range tt.calls {
				if err := c.do(reg, c.name, c.labels, c.value); err != nil {
					t.Fatalf("report on %s: %v", c.name, err)
				}
			}
			got := reg.Gather(ctx)
			if s := samples(family(t, got, tt.family)); !maps.Equal(s, tt.want) {
				t.Errorf("samples = %v, want %v", s, tt.want)
			}
			for i, f := range got {
				if f.Name != tt.family && !reflect.DeepEqual(f, fresh[i]) {
					t.Errorf("family %s changed: %v", f.Name, samples(f))
				}
			}
		})
	}
}

func TestRegistry_ExportsSourcedSamplesAtEveryScrape(t *testing.T) {
	queue := func(status string) map[string]string { return map[string]string{"type": "rebuild", "status": status} }
	ws := func(slug string) map[string]string { return map[string]string{"workspace": slug} }
	jobsZero := zeroSeries([]label{closed("type", "rebuild"),
		closed("status", "queued", "running", "failed", "completed", "dead_letter", "cancelled")}, metrics.Gauge, nil)
	gitZero := zeroSeries([]label{closed("service", "info-refs", "upload-pack", "receive-pack"),
		closed("result", "ok", "refused", "error")}, metrics.Counter, nil)
	with := func(base map[string]float64, key string, v float64) map[string]float64 {
		out := maps.Clone(base)
		out[key] = v
		return out
	}
	tests := []struct {
		name              string
		first, second     []metrics.Sample
		wantFirst, wantSn map[string]float64
	}{
		{"parley_jobqueue_depth",
			[]metrics.Sample{{Labels: queue("queued"), Value: 4}, {Labels: queue("dead_letter"), Value: 1}},
			[]metrics.Sample{{Labels: queue("running"), Value: 2}},
			with(with(jobsZero, "type=rebuild,status=queued", 4), "type=rebuild,status=dead_letter", 1),
			with(jobsZero, "type=rebuild,status=running", 2)},
		{"parley_changes_open",
			[]metrics.Sample{{Labels: ws("ws1"), Value: 2}, {Labels: ws("ws2"), Value: 5}},
			[]metrics.Sample{{Labels: ws("ws3"), Value: 1}},
			map[string]float64{"workspace=ws1": 2, "workspace=ws2": 5},
			map[string]float64{"workspace=ws3": 1}},
		{"parley_git_requests_total",
			[]metrics.Sample{{Labels: map[string]string{"service": "upload-pack", "result": "ok"}, Value: 7}},
			[]metrics.Sample{{Labels: map[string]string{"service": "upload-pack", "result": "ok"}, Value: 8}},
			with(gitZero, "service=upload-pack,result=ok", 7),
			with(gitZero, "service=upload-pack,result=ok", 8)},
		{"parley_forks_active",
			[]metrics.Sample{{Value: 3}}, []metrics.Sample{{Labels: map[string]string{}, Value: 4}},
			map[string]float64{"": 3}, map[string]float64{"": 4}},
	}
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := metrics.New(nil)
			current, calls := tt.first, 0
			err := reg.RegisterSource(tt.name, func(context.Context) ([]metrics.Sample, error) {
				calls++
				return current, nil
			})
			if err != nil {
				t.Fatalf("RegisterSource: %v", err)
			}
			if got := samples(family(t, reg.Gather(ctx), tt.name)); !maps.Equal(got, tt.wantFirst) {
				t.Errorf("first scrape = %v, want %v", got, tt.wantFirst)
			}
			current = tt.second
			if got := samples(family(t, reg.Gather(ctx), tt.name)); !maps.Equal(got, tt.wantSn) {
				t.Errorf("second scrape = %v, want %v", got, tt.wantSn)
			}
			if calls != 2 {
				t.Errorf("source called %d times in two scrapes, want 2", calls)
			}
		})
	}
}

func TestRegistry_SetFamilyReplacesEverySeries(t *testing.T) {
	ws := func(slug string, v float64) metrics.Sample {
		return metrics.Sample{Labels: map[string]string{"workspace": slug}, Value: v}
	}
	kinds := zeroSeries([]label{closed("kind", "hub_event", "run_event", "outcome", "tool_call", "tool_error",
		"trace", "postmortem", "session", "usage")}, metrics.Gauge, nil)
	kinds["kind=session"] = 3
	steps := []struct {
		metric  string
		samples []metrics.Sample
		want    map[string]float64
	}{
		{"parley_agent_sessions_active", []metrics.Sample{ws("ws1", 2), ws("ws2", 1)},
			map[string]float64{"workspace=ws1": 2, "workspace=ws2": 1}},
		{"parley_agent_sessions_active", []metrics.Sample{ws("ws1", 2)}, map[string]float64{"workspace=ws1": 2}},
		{"parley_agent_sessions_active", []metrics.Sample{}, map[string]float64{"workspace=": 0}},
		{"parley_audit_records", []metrics.Sample{{Labels: map[string]string{"kind": "session"}, Value: 3}}, kinds},
	}
	reg := metrics.New(nil)
	for i, s := range steps {
		if err := reg.SetFamily(s.metric, s.samples); err != nil {
			t.Fatalf("step %d: SetFamily(%s): %v", i, s.metric, err)
		}
		if got := samples(family(t, reg.Gather(context.Background()), s.metric)); !maps.Equal(got, s.want) {
			t.Errorf("step %d: samples = %v, want %v", i, got, s.want)
		}
	}
}

func TestRegistry_RefusesAnInvalidCallAndChangesNothing(t *testing.T) {
	landed := map[string]string{"result": "landed"}
	constant := func(v float64) metrics.Source {
		return func(context.Context) ([]metrics.Sample, error) { return []metrics.Sample{{Value: v}}, nil }
	}
	tests := []struct {
		name   string
		metric string
		call   func(*metrics.Registry) error
	}{
		{"a metric outside the catalogue", "parley_nope",
			func(r *metrics.Registry) error { return r.Add("parley_nope", nil, 1) }},
		{"a missing label", "parley_lands_total",
			func(r *metrics.Registry) error { return r.Add("parley_lands_total", nil, 1) }},
		{"an extra label", "parley_lands_total", func(r *metrics.Registry) error {
			return r.Add("parley_lands_total", map[string]string{"result": "landed", "extra": "x"}, 1)
		}},
		{"a value outside a closed set", "parley_lands_total", func(r *metrics.Registry) error {
			return r.Add("parley_lands_total", map[string]string{"result": "maybe"}, 1)
		}},
		{"a negative counter amount", "parley_lands_total",
			func(r *metrics.Registry) error { return r.Add("parley_lands_total", landed, -1) }},
		{"Set on a counter", "parley_lands_total",
			func(r *metrics.Registry) error { return r.Set("parley_lands_total", landed, 1) }},
		{"Add on a gauge", "parley_events_seq",
			func(r *metrics.Registry) error { return r.Add("parley_events_seq", nil, 1) }},
		{"Observe on a gauge", "parley_events_seq",
			func(r *metrics.Registry) error { return r.Observe("parley_events_seq", nil, 1) }},
		{"Add on a histogram", "parley_land_attempts",
			func(r *metrics.Registry) error { return r.Add("parley_land_attempts", nil, 1) }},
		{"SetFamily on a counter", "parley_lands_total", func(r *metrics.Registry) error {
			return r.SetFamily("parley_lands_total", []metrics.Sample{{Labels: landed, Value: 5}})
		}},
		{"SetFamily with one invalid sample", "parley_audit_records", func(r *metrics.Registry) error {
			return r.SetFamily("parley_audit_records", []metrics.Sample{
				{Labels: map[string]string{"kind": "session"}, Value: 3},
				{Labels: map[string]string{"kind": "bogus"}, Value: 1}})
		}},
		{"a source outside the catalogue", "parley_nope",
			func(r *metrics.Registry) error { return r.RegisterSource("parley_nope", constant(1)) }},
		{"a source for a histogram", "parley_land_attempts",
			func(r *metrics.Registry) error { return r.RegisterSource("parley_land_attempts", constant(1)) }},
		{"a nil source", "parley_sse_connections",
			func(r *metrics.Registry) error { return r.RegisterSource("parley_sse_connections", nil) }},
		{"a second source for one metric", "parley_forks_active",
			func(r *metrics.Registry) error { return r.RegisterSource("parley_forks_active", constant(9)) }},
	}
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := metrics.New(nil)
			if err := errors.Join(
				reg.Add("parley_lands_total", landed, 1),
				reg.Observe("parley_land_attempts", nil, 2),
				reg.SetFamily("parley_audit_records", []metrics.Sample{{Labels: map[string]string{"kind": "trace"}, Value: 1}}),
				reg.RegisterSource("parley_forks_active", constant(3)),
			); err != nil {
				t.Fatalf("setup: %v", err)
			}
			before := reg.Gather(ctx)
			err := tt.call(reg)
			if err == nil || !strings.Contains(err.Error(), tt.metric) {
				t.Errorf("err = %v, want an error naming %s", err, tt.metric)
			}
			if after := reg.Gather(ctx); !reflect.DeepEqual(after, before) {
				t.Errorf("a refused call changed the registry")
			}
		})
	}
}

func TestRegistry_ExportsAFailingSourceAtZero(t *testing.T) {
	const metric = "parley_jobqueue_depth"
	queue := func(status string) map[string]string { return map[string]string{"type": "rebuild", "status": status} }
	zero := zeroSeries([]label{closed("type", "rebuild"),
		closed("status", "queued", "running", "failed", "completed", "dead_letter", "cancelled")}, metrics.Gauge, nil)
	withQueued := maps.Clone(zero)
	withQueued["type=rebuild,status=queued"] = 4
	healthy := maps.Clone(zero)
	healthy["type=rebuild,status=running"] = 2
	tests := []struct {
		name string
		fail metrics.Source
		want map[string]float64
	}{
		{"an error", func(context.Context) ([]metrics.Sample, error) {
			return nil, errors.New("database is locked")
		}, zero},
		{"a panic", func(context.Context) ([]metrics.Sample, error) { panic("boom") }, zero},
		{"a value outside a closed set beside a valid sample", func(context.Context) ([]metrics.Sample, error) {
			return []metrics.Sample{{Labels: queue("bogus"), Value: 1}, {Labels: queue("queued"), Value: 4}}, nil
		}, withQueued},
	}
	ctx := context.Background()
	fresh := metrics.New(nil).Gather(ctx)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			reg := metrics.New(slog.New(slog.NewTextHandler(&logs, nil)))
			failing := true
			err := reg.RegisterSource(metric, func(ctx context.Context) ([]metrics.Sample, error) {
				if failing {
					return tt.fail(ctx)
				}
				return []metrics.Sample{{Labels: queue("running"), Value: 2}}, nil
			})
			if err != nil {
				t.Fatalf("RegisterSource: %v", err)
			}
			got := reg.Gather(ctx)
			if len(got) != len(fresh) {
				t.Fatalf("exported %d families, want %d", len(got), len(fresh))
			}
			for i, f := range got {
				if f.Name != metric && !reflect.DeepEqual(f, fresh[i]) {
					t.Errorf("family %s changed: %v", f.Name, samples(f))
				}
			}
			if s := samples(family(t, got, metric)); !maps.Equal(s, tt.want) {
				t.Errorf("samples = %v, want %v", s, tt.want)
			}
			if !strings.Contains(logs.String(), metric) {
				t.Errorf("log %q does not name %s", logs.String(), metric)
			}
			failing = false
			if s := samples(family(t, reg.Gather(ctx), metric)); !maps.Equal(s, healthy) {
				t.Errorf("after recovery samples = %v, want %v", s, healthy)
			}
		})
	}
}

// wellFormed checks what an exposition of the families needs: every
// catalogue family, a label value per label, cumulative buckets within the
// count.
func wellFormed(families []metrics.Family) error {
	if len(families) != len(documented) {
		return fmt.Errorf("%d families", len(families))
	}
	for _, f := range families {
		for _, s := range f.Series {
			if len(s.LabelValues) != len(f.Labels) {
				return fmt.Errorf("%s: %d label values for %d labels", f.Name, len(s.LabelValues), len(f.Labels))
			}
			if f.Type != metrics.Histogram {
				continue
			}
			if len(s.Buckets) != len(f.Buckets) {
				return fmt.Errorf("%s: %d bucket counts for %d bounds", f.Name, len(s.Buckets), len(f.Buckets))
			}
			for i, n := range s.Buckets {
				if n > s.Count || (i > 0 && n < s.Buckets[i-1]) {
					return fmt.Errorf("%s: buckets %v not cumulative within count %d", f.Name, s.Buckets, s.Count)
				}
			}
		}
	}
	return nil
}

func TestRegistry_AppliesConcurrentReportsExactlyOnce(t *testing.T) {
	const reporters, reports, scrapers = 50, 200, 5
	reg := metrics.New(nil)
	landed := map[string]string{"result": "landed"}
	ctx := context.Background()
	if err := reg.RegisterSource("parley_forks_active", func(context.Context) ([]metrics.Sample, error) {
		return []metrics.Sample{{Value: 1}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg, scrapes sync.WaitGroup
	done := make(chan struct{})
	errs := make(chan error, reporters*reports+scrapers)
	for range scrapers {
		scrapes.Go(func() {
			for {
				if err := wellFormed(reg.Gather(ctx)); err != nil {
					errs <- err
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		})
	}
	for range reporters {
		wg.Go(func() {
			for range reports {
				if err := errors.Join(reg.Add("parley_lands_total", landed, 1),
					reg.Observe("parley_land_attempts", nil, 1)); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(done)
	scrapes.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	final := reg.Gather(ctx)
	if got := samples(family(t, final, "parley_lands_total"))["result=landed"]; got != reporters*reports {
		t.Errorf("landed = %v, want %d", got, reporters*reports)
	}
	if got := samples(family(t, final, "parley_land_attempts"))["|count"]; got != reporters*reports {
		t.Errorf("attempts count = %v, want %d", got, reporters*reports)
	}
}
