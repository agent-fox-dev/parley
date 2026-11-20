package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestLogging_ATraceLogParsesAttributesEveryRequestAndHoldsNoSecret(t *testing.T) {
	parallel(t)
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n[logging]\nlevel = \"trace\"\nlog_health_probes = true\n")
	served := hubtest.Serve(t, hub.Options{WorkDir: dir})

	var markers [5]string
	for i := range markers {
		markers[i] = rand.Text()[:24]
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x:" + markers[1]))
	requests := []struct {
		method, path string
		header       http.Header
		body         any
	}{
		{http.MethodGet, "/api/v1/user", http.Header{"Authorization": {"Bearer " + markers[0]}}, nil},
		{http.MethodGet, "/git/acme/app.git/info/refs?service=git-upload-pack", http.Header{"Authorization": {"Basic " + basic}}, nil},
		{http.MethodGet, "/version", http.Header{"Cookie": {"session=" + markers[2]}}, nil},
		{http.MethodPost, "/api/v1/no-such-route", nil, map[string]string{"secret": markers[3]}},
		{http.MethodGet, "/healthz?token=" + markers[4], nil, nil},
	}
	var ids []string
	for _, r := range requests {
		_, header, _ := served.Exchange(t, r.method, r.path, "", r.body, r.header)
		id := header.Get("X-Request-ID")
		if id == "" {
			t.Fatalf("%s %s: the response has no X-Request-ID", r.method, r.path)
		}
		ids = append(ids, id)
	}
	served.Stop()
	if code := served.ExitCode(t, 10*time.Second); code != 0 {
		t.Errorf("the hub exited %d after its stop, want 0", code)
	}

	stderr := served.Log.String()
	accessLines := map[string]bool{}
	for line := range strings.Lines(stderr) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %q is not a JSON object: %v", line, err)
			continue
		}
		_, isMsg := rec["msg"].(string)
		if rec["time"] == "" || rec["time"] == nil || rec["level"] == "" || rec["level"] == nil || !isMsg {
			t.Errorf("line %q lacks time, level or a string msg", line)
		}
		_, hasMethod := rec["method"]
		_, hasPath := rec["path"]
		_, hasStatus := rec["status"]
		if id, ok := rec["request_id"].(string); ok && hasMethod && hasPath && hasStatus {
			accessLines[id] = true
		}
	}
	for i, id := range ids {
		if !accessLines[id] {
			t.Errorf("%s %s: no access-log line carries its request id %s", requests[i].method, requests[i].path, id)
		}
	}
	for i, m := range markers {
		if strings.Contains(stderr, m) {
			t.Errorf("the log holds marker %d, %s", i+1, m)
		}
	}
	if strings.Contains(stderr, basic) {
		t.Error("the log holds the Basic credential base64-encoded")
	}
}
