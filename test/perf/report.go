package perf

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Report is what a run measured: the hub, the run, one result per scenario
// in the fixed order, and the clean-up requests that failed.
type Report struct {
	Hub           Hub       `json:"hub"`
	RunID         string    `json:"run_id"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	Results       []Result  `json:"results"`
	CleanupErrors []string  `json:"cleanup_errors"`
}

// Hub is the hub a run measured: its URL and the document its GET /version
// answered.
type Hub struct {
	URL     string          `json:"url"`
	Version json.RawMessage `json:"version"`
}

// Result is a scenario's measurement and verdict. Measured is nil when the
// scenario has no measurement; Detail holds at most 200 bytes.
type Result struct {
	Name     string   `json:"name"`
	Target   Target   `json:"target"`
	Measured *float64 `json:"measured"`
	Samples  int      `json:"samples"`
	Status   Status   `json:"status"`
	Detail   string   `json:"detail"`
}

// write writes r as JSON to path, and names the path when it cannot.
func (r Report) write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the report for %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write the report to %s: %w", path, err)
	}
	return nil
}

// comparisonSigns are the signs of the comparisons in the table.
var comparisonSigns = map[string]string{LT: "<", LE: "<=", GE: ">="}

// Table is the results as a text table, one row per scenario, followed by
// the clean-up requests that failed.
func (r Report) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hub %s, run %s\n", r.Hub.URL, r.RunID)
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SCENARIO\tSTATUS\tMEASURED\tTARGET\tSAMPLES\tDETAIL")
	for _, res := range r.Results {
		measured := "-"
		if res.Measured != nil {
			measured = formatValue(*res.Measured)
		}
		target := fmt.Sprintf("%s %s %s", comparisonSigns[res.Target.Comparison], formatValue(res.Target.Value), res.Target.Unit)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", res.Name, res.Status, measured, target, res.Samples, res.Detail)
	}
	_ = w.Flush()
	for _, e := range r.CleanupErrors {
		fmt.Fprintf(&b, "clean-up failed: %s\n", e)
	}
	return b.String()
}

// formatValue is v rounded to three decimals.
func formatValue(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}
