package hub

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/scrub"
)

func TestNewLogger_DropsLinesBelowTheConfiguredLevel(t *testing.T) {
	order := []string{"trace", "debug", "info", "warn", "error"}
	for i, level := range order {
		t.Run(level, func(t *testing.T) {
			var buf syncBuffer
			log := NewLogger(&buf, level, scrub.New())
			for _, l := range order {
				log.Log(context.Background(), levelOf(l), "m-"+l)
			}
			var got []string
			for _, line := range nonEmptyLines(buf.String()) {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("line %q is not JSON", line)
				}
				for _, k := range []string{"time", "level", "msg"} {
					if _, ok := rec[k]; !ok {
						t.Errorf("line %s lacks %s", line, k)
					}
				}
				got = append(got, rec["msg"].(string))
			}
			var want []string
			for _, l := range order[i:] {
				want = append(want, "m-"+l)
			}
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("lines = %v, want %v", got, want)
			}
		})
	}
}

func TestNewLogger_ScrubsTheMessageAndStringAttributes(t *testing.T) {
	s := scrub.New()
	s.Add("hunter2")
	var buf syncBuffer
	log := NewLogger(&buf, "info", s)
	pat := "parley_pat_abcd1234_" + strings.Repeat("0123456789abcdef", 2)
	log.Info("clone failed for https://u:hunter2@h/r.git", "err", "bad token "+pat, "attempt", 3)
	var rec map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "clone failed for https://h/r.git" {
		t.Errorf("msg = %v", rec["msg"])
	}
	if rec["err"] != "bad token [redacted]" {
		t.Errorf("err = %v", rec["err"])
	}
	if rec["attempt"] != float64(3) || rec["level"] != "INFO" {
		t.Errorf("attempt %v level %v", rec["attempt"], rec["level"])
	}
}
