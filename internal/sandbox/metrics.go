package sandbox

import "github.com/agent-fox-dev/parley/internal/sandbox/runtime"

// Recorder is what the sandbox packages report their metrics to: Add, Set
// and Observe with a metric name, a label map and a value, each returning an
// error, the method set of the hub's metrics registry, so that the registry
// is passed as it is. It is runtime.Recorder under the feature's name, so
// that one value serves the runtime client, which reports
// parley_sandbox_runtime_up on every change of its reachability flag, and
// the rest of the feature.
type Recorder = runtime.Recorder

// NopRecorder records nothing and returns no error. It is the recorder of a
// hub that has none.
type NopRecorder = runtime.NopRecorder
