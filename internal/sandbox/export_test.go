package sandbox

import "net/http"

// useEntryOf is the entry of the route that uses a sandbox named name
// (exec, upload, download, logs or attach).
func useEntryOf(s *Service, name string) *useEntry {
	for _, e := range s.use {
		if e.name == name {
			return e
		}
	}
	panic("no route that uses a sandbox is named " + name)
}

// SetUseOperation makes op the operation of the route that uses a sandbox
// named name, so that a test drives the checks before it without the
// operation behind it. It is called while the hub composes.
func SetUseOperation(s *Service, name string, op func(http.ResponseWriter, *http.Request) error) {
	useEntryOf(s, name).operate = func(w http.ResponseWriter, r *http.Request, _ useRequest) error { return op(w, r) }
}

// SetUseValidation makes v the request validation of the route that uses a
// sandbox named name. It is called while the hub composes.
func SetUseValidation(s *Service, name string, v func(*http.Request) error) {
	useEntryOf(s, name).validate = func(r *http.Request, _ *useRequest) error { return v(r) }
}

// InsertLogLines stores lines as the log lines of the sandbox sandboxID in
// tx, as the collector does, so that a test sets up lines of any time.
var InsertLogLines = insertLogLines
