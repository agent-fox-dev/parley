package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/server"
)

// maxLabel is the longest label (node_id, tool_name), in characters.
const maxLabel = 255

// runIDForm is the form of a run id: YYYYMMDD_HHMMSS_ and six lower-case
// hexadecimal digits.
var runIDForm = regexp.MustCompile(`^[0-9]{8}_[0-9]{6}_[0-9a-f]{6}$`)

// runIDRule is the rule a run id breaks when it does not have runIDForm,
// and msgRunID the refusal of such a run id in a path.
const (
	runIDRule = "must have the form YYYYMMDD_HHMMSS_ and six lower-case hexadecimal digits"
	msgRunID  = "run_id " + runIDRule
)

// fields are the members of a record body, by name. Its readers treat a
// member that is absent and one that is JSON null alike, and a violation is
// 400 invalid_request with a message that starts with the member's name.
// Members no reader asks for are ignored.
type fields map[string]json.RawMessage

// invalid is the refusal of the member name, which breaks rule.
func invalid(name, rule string) *server.Error {
	return server.InvalidRequest(name + " " + rule)
}

// absent reports whether the member name is absent or null.
func (f fields) absent(name string) bool {
	raw := f[name]
	return len(raw) == 0 || string(raw) == "null"
}

// str reads the member name as a string, nil when absent.
func (f fields) str(name string) (*string, error) {
	if f.absent(name) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(f[name], &s); err != nil {
		return nil, invalid(name, "must be a string")
	}
	return &s, nil
}

// required reads the member name as a non-empty string.
func (f fields) required(name string) (string, error) {
	s, err := f.str(name)
	if err != nil {
		return "", err
	}
	if s == nil || *s == "" {
		return "", invalid(name, "is required and must be a non-empty string")
	}
	return *s, nil
}

// oneOf reads the member name as one of the strings allowed, nil when
// absent.
func (f fields) oneOf(name string, allowed ...string) (*string, error) {
	s, err := f.str(name)
	if err != nil || s == nil {
		return s, err
	}
	if !slices.Contains(allowed, *s) {
		return nil, invalid(name, "must be one of "+strings.Join(allowed, ", "))
	}
	return s, nil
}

// label reads the member name as a string of at most maxLabel characters,
// nil when absent.
func (f fields) label(name string) (*string, error) {
	s, err := f.str(name)
	if err != nil || s == nil {
		return s, err
	}
	if utf8.RuneCountInString(*s) > maxLabel {
		return nil, invalid(name, fmt.Sprintf("must be at most %d characters", maxLabel))
	}
	return s, nil
}

// uuid reads the member name as a UUID in any letter case and returns it in
// lower case, nil when absent.
func (f fields) uuid(name string) (*string, error) {
	s, err := f.str(name)
	if err != nil || s == nil {
		return s, err
	}
	id, ok := normaliseUUID(*s)
	if !ok {
		return nil, invalid(name, "must be a UUID in the 8-4-4-4-12 hexadecimal form")
	}
	return &id, nil
}

// timestamp reads the member name as an RFC 3339 timestamp (parseTimestamp),
// nil when absent.
func (f fields) timestamp(name string) (*time.Time, error) {
	s, err := f.str(name)
	if err != nil {
		return nil, invalid(name, "must be an RFC 3339 timestamp string")
	}
	if s == nil {
		return nil, nil
	}
	t, ok := parseTimestamp(*s)
	if !ok {
		return nil, invalid(name, "must be an RFC 3339 timestamp whose year in UTC is 0000 to 9999")
	}
	return &t, nil
}

// count reads the member name as a whole number of at least 0, nil when
// absent. A fraction, an exponent, a number in a string and a minus sign,
// also before 0, are refused.
func (f fields) count(name string) (*int64, error) {
	if f.absent(name) {
		return nil, nil
	}
	raw := string(f[name])
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strings.HasPrefix(raw, "-") {
		return nil, invalid(name, "must be a whole number of at least 0")
	}
	return &n, nil
}

// object reads the member name as a JSON object, compacted, nil when absent.
func (f fields) object(name string) (json.RawMessage, error) {
	if f.absent(name) {
		return nil, nil
	}
	raw := f[name]
	if raw[0] != '{' {
		return nil, invalid(name, "must be a JSON object")
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, invalid(name, "must be a JSON object")
	}
	return b.Bytes(), nil
}

// runID checks the run_id member against the run id of the path, which a
// body may repeat but not contradict.
func (f fields) runID(path string) error {
	s, err := f.str("run_id")
	if err != nil {
		return err
	}
	if s != nil && *s != path {
		return invalid("run_id", "of the body must equal the run_id of the path")
	}
	return nil
}

// normaliseUUID reports whether s is a UUID in the 8-4-4-4-12 hexadecimal
// form, of any version and letter case, and returns it in lower case.
func normaliseUUID(s string) (string, bool) {
	if !server.IsUUID(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// parseTimestamp reads the client timestamp s, RFC 3339 with any offset,
// as an instant in UTC, cut to the microsecond: the precision a stored
// timestamp keeps. It refuses a fraction of a second after a comma, which
// time.Parse takes and RFC 3339 does not, and an instant whose year in UTC
// is not 0000 to 9999, which storedLayout cannot hold in its four digits
// and so could not be read back.
func parseTimestamp(s string) (time.Time, bool) {
	if strings.Contains(s, ",") {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	t = t.UTC().Truncate(time.Microsecond)
	if y := t.Year(); y < 0 || y > 9999 {
		return time.Time{}, false
	}
	return t, true
}

// storedLayout is the form a timestamp is stored in: fixed width, so that
// the text of a column sorts as the instants do.
const storedLayout = "2006-01-02T15:04:05.000000Z"

// clientTime is a client timestamp member of a response: RFC 3339 in UTC
// with the suffix Z and the fraction of a second it has, without trailing
// zeros. Hub times are timefmt.Time, in whole seconds.
type clientTime time.Time

// String writes the timestamp as a response does.
func (t clientTime) String() string {
	return time.Time(t).UTC().Format(time.RFC3339Nano)
}

// MarshalJSON writes the timestamp as a JSON string in the form of String.
func (t clientTime) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, t.String()), nil
}

// stored is the timestamp in the form of storedLayout.
func (t clientTime) stored() string {
	return time.Time(t).UTC().Format(storedLayout)
}

// ValidRunID reports whether s has the form of a run id: YYYYMMDD_HHMMSS_
// and six lower-case hexadecimal digits.
func ValidRunID(s string) bool { return runIDForm.MatchString(s) }

// Severities are the severities of a run event, from the least severe.
func Severities() []string {
	return []string{severityInfo, severityWarning, severityError, severityCritical}
}

// Query is the query string of a read, whose parameters the reader takes
// one at a time: each method reads one parameter, checks it, and returns
// nil when the parameter is absent. A parameter that is present is given,
// even when empty. The first violation is kept and Err returns it, a 400
// invalid_request whose message starts with the parameter's name, so that
// a read is refused before it reads anything. Parameters no method reads
// are ignored, as unknown body members are.
type Query struct {
	values url.Values
	err    error
}

// ReadQuery is the query string of r. One that does not parse is refused.
func ReadQuery(r *http.Request) *Query {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return &Query{err: server.InvalidRequest("the query string is malformed")}
	}
	return &Query{values: values}
}

// Err is the first violation the parameters read so far hold, nil when
// there is none.
func (q *Query) Err() error { return q.err }

// fail keeps err unless an earlier violation is kept.
func (q *Query) fail(err *server.Error) {
	if q.err == nil {
		q.err = err
	}
}

// String reads the parameter name as it is, for a filter that compares
// exactly.
func (q *Query) String(name string) *string {
	if !q.values.Has(name) {
		return nil
	}
	s := q.values.Get(name)
	return &s
}

// OneOf reads the parameter name, which must be one of allowed.
func (q *Query) OneOf(name string, allowed ...string) *string {
	s := q.String(name)
	if s != nil && !slices.Contains(allowed, *s) {
		q.fail(invalid(name, "must be one of "+strings.Join(allowed, ", ")))
		return nil
	}
	return s
}

// UUID reads the parameter name as a UUID in any letter case and returns it
// in lower case, the form ids are stored in.
func (q *Query) UUID(name string) *string {
	s := q.String(name)
	if s == nil {
		return nil
	}
	id, ok := normaliseUUID(*s)
	if !ok {
		q.fail(invalid(name, "must be a UUID in the 8-4-4-4-12 hexadecimal form"))
		return nil
	}
	return &id
}

// RunID reads the parameter name as a run id (ValidRunID).
func (q *Query) RunID(name string) *string {
	s := q.String(name)
	if s != nil && !ValidRunID(*s) {
		q.fail(invalid(name, runIDRule))
		return nil
	}
	return s
}

// Time reads the parameter name as an RFC 3339 timestamp with any offset,
// as an instant in UTC cut to the microsecond (parseTimestamp).
func (q *Query) Time(name string) *time.Time {
	s := q.String(name)
	if s == nil {
		return nil
	}
	t, ok := parseTimestamp(*s)
	if !ok {
		q.fail(invalid(name, "must be an RFC 3339 timestamp whose year in UTC is 0000 to 9999"))
		return nil
	}
	return &t
}

// Range reads the half-open time range since, until: a record is in it
// when its time is at or after since and before until. When both are
// given, since must be before until.
func (q *Query) Range() (since, until *time.Time) {
	since, until = q.Time("since"), q.Time("until")
	if since != nil && until != nil && !since.Before(*until) {
		q.fail(invalid("since", "must be before until"))
		return nil, nil
	}
	return since, until
}

// Descending reads the parameter order, asc (the default) or desc, and
// reports whether it is desc.
func (q *Query) Descending() bool {
	order := q.OneOf("order", "asc", "desc")
	return order != nil && *order == "desc"
}
