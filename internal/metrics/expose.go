package metrics

import (
	"io"
	"math"
	"strconv"
	"strings"
)

// WriteText writes families in the Prometheus text exposition format 0.0.4,
// in their order: each family's # HELP and # TYPE lines, then its samples in
// the order of its series. A histogram's series is written as its _bucket
// samples, each with the cumulative count at or below its le bound and the
// last at +Inf, then its _sum and its _count.
func WriteText(w io.Writer, families []Family) error {
	var b strings.Builder
	for _, f := range families {
		b.WriteString("# HELP " + f.Name + " " + helpEscaper.Replace(f.Help) + "\n")
		b.WriteString("# TYPE " + f.Name + " " + string(f.Type) + "\n")
		for _, s := range f.Series {
			if f.Type != Histogram {
				writeSample(&b, f.Name, f.Labels, s.LabelValues, "", formatValue(s.Value))
				continue
			}
			for i, bound := range f.Buckets {
				writeSample(&b, f.Name+"_bucket", f.Labels, s.LabelValues, formatValue(bound), strconv.FormatUint(s.Buckets[i], 10))
			}
			writeSample(&b, f.Name+"_bucket", f.Labels, s.LabelValues, "+Inf", strconv.FormatUint(s.Count, 10))
			writeSample(&b, f.Name+"_sum", f.Labels, s.LabelValues, "", formatValue(s.Sum))
			writeSample(&b, f.Name+"_count", f.Labels, s.LabelValues, "", strconv.FormatUint(s.Count, 10))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// The format escapes a backslash and a line feed in a help text, and a
// double quote too in a label value, so that every sample stays one line.
var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

// writeSample writes one sample line: name, the labels with their values
// and, when le is not "", the bucket bound as the last label.
func writeSample(b *strings.Builder, name string, labels []Label, values []string, le, value string) {
	b.WriteString(name)
	if len(labels) > 0 || le != "" {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l.Name + `="` + labelEscaper.Replace(values[i]) + `"`)
		}
		if le != "" {
			if len(labels) > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`le="` + le + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteString(" " + value + "\n")
}

// formatValue writes v as the format reads it: an integer in full, without
// an exponent, so that a counter or a Unix time reads as it is, anything
// else in the shortest form that reads back the same.
func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	case v == math.Trunc(v):
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
