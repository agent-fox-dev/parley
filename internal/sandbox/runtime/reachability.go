package runtime

import "sync"

// MetricUp is the gauge the reachability flag is reported to: 1 while the
// runtime is reachable, 0 while it is not.
const MetricUp = "parley_sandbox_runtime_up"

// Recorder is what the sandbox packages report their metrics to. It has the
// method set of the hub's metrics registry, so the registry is passed as it
// is. It is declared here, not in internal/sandbox, because the runtime
// clients report to it and internal/sandbox imports this package.
type Recorder interface {
	Add(name string, labels map[string]string, value float64) error
	Set(name string, labels map[string]string, value float64) error
	Observe(name string, labels map[string]string, value float64) error
}

// NopRecorder records nothing. It is the recorder when the hub has none.
type NopRecorder struct{}

// Add records nothing.
func (NopRecorder) Add(string, map[string]string, float64) error { return nil }

// Set records nothing.
func (NopRecorder) Set(string, map[string]string, float64) error { return nil }

// Observe records nothing.
func (NopRecorder) Observe(string, map[string]string, float64) error { return nil }

// Reachability is a client's reachability flag. A result of kind
// ErrUnavailable clears it, any other result sets it, a success and the
// kinds ErrNotFound, ErrInvalid and ErrUnsupported included, and every change
// is reported to the recorder as MetricUp 1 or 0. The flag starts set: the
// hub serves only after its start-up check succeeded through the client.
// Every implementation of Client keeps its flag in one, so that the rule
// lives in one place. It is safe for concurrent use.
type Reachability struct {
	mu   sync.Mutex
	down bool
	rec  Recorder
}

// NewReachability returns a set flag that reports its changes to rec; nil
// reports them nowhere.
func NewReachability(rec Recorder) *Reachability {
	if rec == nil {
		rec = NopRecorder{}
	}
	return &Reachability{rec: rec}
}

// Reachable reports the flag.
func (r *Reachability) Reachable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.down
}

// Observe moves the flag on an operation's result and returns err. A change
// is reported while the flag is held, so the recorder hears the changes in
// the order they happened, each once. The recorder's error is dropped: a
// metric that cannot be recorded does not fail the operation.
func (r *Reachability) Observe(err error) error {
	down := err != nil && KindOf(err) == ErrUnavailable
	r.mu.Lock()
	defer r.mu.Unlock()
	if down != r.down {
		r.down = down
		value := 1.0
		if down {
			value = 0
		}
		_ = r.rec.Set(MetricUp, nil, value)
	}
	return err
}
