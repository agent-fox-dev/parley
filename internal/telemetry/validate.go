package telemetry

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
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

// msgRunID is the refusal of a run id that does not have runIDForm.
const msgRunID = "run_id must have the form YYYYMMDD_HHMMSS_ and six lower-case hexadecimal digits"

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
	if len(s) != 36 {
		return "", false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return "", false
			}
		}
	}
	return strings.ToLower(s), true
}

// newID returns a random (version 4) UUID in lower case.
func newID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error; it ends the program when the
	// system's random source fails.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
