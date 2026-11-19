package metrics

// Recorder is what a package reports its metrics to. Add grows a counter,
// Set sets a gauge, Observe adds an observation to a histogram, each with
// the metric's name, one value for each of its labels (nil for a metric
// without labels) and the value; SetFamily replaces every series of a gauge.
// A report that does not fit the catalogue returns an error naming the
// metric and records nothing. Add, Set and Observe have the signatures of
// the sandbox packages' recorder, so that a Registry is passed to them as it
// is.
type Recorder interface {
	Add(name string, labels map[string]string, value float64) error
	Set(name string, labels map[string]string, value float64) error
	Observe(name string, labels map[string]string, value float64) error
	SetFamily(name string, samples []Sample) error
}

// Discard accepts every report and records nothing: the recorder of a
// component built without a registry, in the tests of other packages.
var Discard Recorder = discard{}

type discard struct{}

func (discard) Add(string, map[string]string, float64) error     { return nil }
func (discard) Set(string, map[string]string, float64) error     { return nil }
func (discard) Observe(string, map[string]string, float64) error { return nil }
func (discard) SetFamily(string, []Sample) error                 { return nil }

var _ Recorder = (*Registry)(nil)
