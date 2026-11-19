package metrics_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/metrics"
)

// parsed is one family of a text exposition: its HELP and TYPE lines, as
// many as there were, and its samples in the order written.
type parsed struct {
	name    string
	help    []string
	typ     []string
	samples []sample
}

// sample is one sample line: the sample's name, its labels in the order
// written and its value.
type sample struct {
	name   string
	labels [][2]string
	value  float64
}

func (s sample) label(name string) string {
	for _, l := range s.labels {
		if l[0] == name {
			return l[1]
		}
	}
	return ""
}

// parseText reads a text exposition (format 0.0.4) strictly: each family
// starts with its comment lines, and every sample belongs to the family
// before it (a histogram's as _bucket, _sum and _count).
func parseText(text string) ([]parsed, error) {
	if !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("the exposition does not end in a line feed")
	}
	var out []parsed
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			kind, rest, _ := strings.Cut(rest, " ")
			name, text, _ := strings.Cut(rest, " ")
			if len(out) == 0 || out[len(out)-1].name != name {
				if len(out) > 0 && len(out[len(out)-1].samples) == 0 {
					return nil, fmt.Errorf("line %d: family %s has no sample", i+1, out[len(out)-1].name)
				}
				out = append(out, parsed{name: name})
			}
			f := &out[len(out)-1]
			switch {
			case len(f.samples) > 0:
				return nil, fmt.Errorf("line %d: a comment after the samples of %s", i+1, name)
			case kind == "HELP":
				f.help = append(f.help, text)
			case kind == "TYPE":
				f.typ = append(f.typ, text)
			default:
				return nil, fmt.Errorf("line %d: comment %q", i+1, line)
			}
			continue
		}
		s, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if len(out) == 0 || len(out[len(out)-1].typ) == 0 {
			return nil, fmt.Errorf("line %d: a sample before its family's TYPE line", i+1)
		}
		f := &out[len(out)-1]
		own := []string{f.name}
		if f.typ[0] == "histogram" {
			own = []string{f.name + "_bucket", f.name + "_sum", f.name + "_count"}
		}
		if !slices.Contains(own, s.name) {
			return nil, fmt.Errorf("line %d: sample %s in family %s", i+1, s.name, f.name)
		}
		f.samples = append(f.samples, s)
	}
	return out, nil
}

func parseSample(line string) (sample, error) {
	var s sample
	end := strings.IndexAny(line, "{ ")
	if end <= 0 {
		return s, fmt.Errorf("sample %q", line)
	}
	s.name, line = line[:end], line[end:]
	if rest, ok := strings.CutPrefix(line, "{"); ok {
		for !strings.HasPrefix(rest, "}") {
			name, value, ok := strings.Cut(rest, `="`)
			if !ok {
				return s, fmt.Errorf("labels %q", rest)
			}
			var b strings.Builder
			i := 0
			for ; i < len(value) && value[i] != '"'; i++ {
				c := value[i]
				if c == '\\' && i+1 < len(value) {
					i++
					switch value[i] {
					case 'n':
						c = '\n'
					case '\\', '"':
						c = value[i]
					default:
						return s, fmt.Errorf("escape \\%c", value[i])
					}
				}
				b.WriteByte(c)
			}
			if i == len(value) {
				return s, fmt.Errorf("unterminated label value in %q", rest)
			}
			s.labels = append(s.labels, [2]string{name, b.String()})
			rest = strings.TrimPrefix(value[i+1:], ",")
		}
		line = rest[1:]
	}
	v, err := strconv.ParseFloat(strings.TrimPrefix(line, " "), 64)
	if err != nil || !strings.HasPrefix(line, " ") {
		return s, fmt.Errorf("value %q", line)
	}
	s.value = v
	return s, nil
}

// expose writes the registry's exposition.
func expose(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := metrics.WriteText(&b, reg.Gather(context.Background())); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// block is the lines the exposition text writes for the family name.
func block(text, name string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		n := strings.TrimPrefix(strings.TrimPrefix(line, "# HELP "), "# TYPE ")
		if n == name || strings.HasPrefix(n, name+" ") || strings.HasPrefix(n, name+"{") ||
			strings.HasPrefix(n, name+"_bucket") || strings.HasPrefix(n, name+"_sum") || strings.HasPrefix(n, name+"_count") {
			out = append(out, line)
		}
	}
	return out
}

func TestWriteText_EscapesLabelValues(t *testing.T) {
	t.Parallel()
	for _, v := range []string{`a"b`, `a\b`, "line1\nline2", "plain", "é"} {
		t.Run(strconv.Quote(v), func(t *testing.T) {
			t.Parallel()
			reg := metrics.New(nil)
			if err := reg.Add(metrics.AgentTokensTotal, map[string]string{"workspace": "ws1", "model": v, "direction": "input"}, 1); err != nil {
				t.Fatal(err)
			}
			text := expose(t, reg)
			lines := block(text, metrics.AgentTokensTotal)
			if len(lines) != 3 {
				t.Fatalf("the family takes %d lines, want HELP, TYPE and one sample:\n%s", len(lines), strings.Join(lines, "\n"))
			}
			families, err := parseText(text)
			if err != nil {
				t.Fatalf("parse: %v\n%s", err, text)
			}
			for _, f := range families {
				if f.name == metrics.AgentTokensTotal {
					if got := f.samples[0].label("model"); got != v {
						t.Errorf("model = %q, want %q", got, v)
					}
					return
				}
			}
			t.Fatalf("no family %s", metrics.AgentTokensTotal)
		})
	}
}

func TestWriteText_WritesSamplesInTheTextFormat(t *testing.T) {
	t.Parallel()
	reg := metrics.New(nil)
	for _, err := range []error{
		reg.Observe(metrics.LandAttempts, nil, 3),
		reg.Observe(metrics.HTTPRequestDurationSeconds, map[string]string{"method": "GET", "path": "/x"}, 0.03),
		reg.Add(metrics.AgentTokensTotal, map[string]string{"workspace": "w", "model": "m", "direction": "output"}, 12345678),
		reg.Set(metrics.RetentionLastRunTimestamp, nil, 1780315200),
		reg.Set(metrics.ChangesOpen, map[string]string{"workspace": "b"}, 2),
		reg.Set(metrics.ChangesOpen, map[string]string{"workspace": "a"}, 0.5),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	text := expose(t, reg)
	for _, tc := range []struct {
		family string
		want   []string
	}{
		{metrics.LandAttempts, []string{
			"# HELP parley_land_attempts Compare-and-swap attempts per land.",
			"# TYPE parley_land_attempts histogram",
			`parley_land_attempts_bucket{le="1"} 0`,
			`parley_land_attempts_bucket{le="2"} 0`,
			`parley_land_attempts_bucket{le="3"} 1`,
			`parley_land_attempts_bucket{le="5"} 1`,
			`parley_land_attempts_bucket{le="8"} 1`,
			`parley_land_attempts_bucket{le="13"} 1`,
			`parley_land_attempts_bucket{le="+Inf"} 1`,
			"parley_land_attempts_sum 3",
			"parley_land_attempts_count 1",
		}},
		{metrics.HTTPRequestDurationSeconds, []string{
			"# HELP parley_http_request_duration_seconds Time to answer an HTTP request, in seconds.",
			"# TYPE parley_http_request_duration_seconds histogram",
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.005"} 0`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.01"} 0`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.025"} 0`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.05"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.1"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.25"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.5"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="1"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="2.5"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="5"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="10"} 1`,
			`parley_http_request_duration_seconds_bucket{method="GET",path="/x",le="+Inf"} 1`,
			`parley_http_request_duration_seconds_sum{method="GET",path="/x"} 0.03`,
			`parley_http_request_duration_seconds_count{method="GET",path="/x"} 1`,
		}},
		{metrics.AgentTokensTotal, []string{
			"# HELP parley_agent_tokens_total Tokens reported in agent usage records.",
			"# TYPE parley_agent_tokens_total counter",
			`parley_agent_tokens_total{workspace="w",model="m",direction="output"} 12345678`,
		}},
		{metrics.RetentionLastRunTimestamp, []string{
			"# HELP parley_retention_last_run_timestamp_seconds Unix time of the last fully successful retention run.",
			"# TYPE parley_retention_last_run_timestamp_seconds gauge",
			"parley_retention_last_run_timestamp_seconds 1780315200",
		}},
		{metrics.ChangesOpen, []string{
			"# HELP parley_changes_open Changes in state draft or open.",
			"# TYPE parley_changes_open gauge",
			`parley_changes_open{workspace="a"} 0.5`,
			`parley_changes_open{workspace="b"} 2`,
		}},
	} {
		t.Run(tc.family, func(t *testing.T) {
			if got := block(text, tc.family); !slices.Equal(got, tc.want) {
				t.Errorf("lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}
