package telemetry

import "encoding/json"

// NormaliseUUID is normaliseUUID, for the tests of package telemetry_test.
func NormaliseUUID(s string) (string, bool) { return normaliseUUID(s) }

// NormaliseTimestamp parses the client timestamp s and renders it as a
// response does.
func NormaliseTimestamp(s string) (string, bool) {
	t, ok := parseTimestamp(s)
	if !ok {
		return "", false
	}
	return clientTime(t).String(), true
}

// Count reads the JSON value raw as the integer member duration_ms of a
// body.
func Count(raw string) (*int64, error) {
	return fields{"duration_ms": json.RawMessage(raw)}.count("duration_ms")
}
