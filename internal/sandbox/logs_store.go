package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// The sources of a stored log line.
const (
	LogSourceProcess = "process"
	LogSourcePolicy  = "policy"
)

// MaxLogLine is the most bytes one stored log line holds: a longer line is
// stored as consecutive lines of at most this length.
const MaxLogLine = 16384

// LogSchemaStep creates the stored log lines of the sandboxes: one row per
// line, numbered in the order of storage, which is the order lines are
// read in. Times are Unix seconds. A line names its sandbox without a
// foreign key: the purge hook removes the lines with the record, and the
// retention task removes those of a record removed without it.
var LogSchemaStep = store.Step{
	Name:   "sandbox_logs",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sandbox_logs (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	sandbox_id TEXT NOT NULL,
	time       INTEGER NOT NULL,
	source     TEXT NOT NULL,
	stream     TEXT,
	line       TEXT NOT NULL,
	action     TEXT,
	program    TEXT,
	host       TEXT,
	port       INTEGER,
	credential TEXT
);
CREATE INDEX sandbox_logs_sandbox ON sandbox_logs (sandbox_id, source, seq);
CREATE INDEX sandbox_logs_time ON sandbox_logs (time);`)
		return err
	},
}

// LogLine is a stored line of a sandbox's log: a line of its main process's
// output or an entry of its policy log.
type LogLine struct {
	// Seq is the line's place in the order of storage.
	Seq int64
	// Time is the time the runtime gave the line, in whole seconds, UTC.
	Time time.Time
	// Source is LogSourceProcess or LogSourcePolicy.
	Source string
	// Stream is "stdout" or "stderr" for a process line, "" for a policy
	// line.
	Stream string
	// Line is the text, without its line end.
	Line string
	// Action, Program, Host and Port are those of a policy entry, and
	// Credential the provider of a credentialed request, "" for none.
	Action     string
	Program    string
	Host       string
	Port       int
	Credential string
}

// MarshalJSON writes the line as an item of a sandbox's logs: time, source,
// stream and line for a process line; for a policy line stream is null and
// action, program, host and port follow, with credential only for a
// credentialed request.
func (l LogLine) MarshalJSON() ([]byte, error) {
	if l.Source != LogSourcePolicy {
		return json.Marshal(struct {
			Time   timefmt.Time `json:"time"`
			Source string       `json:"source"`
			Stream string       `json:"stream"`
			Line   string       `json:"line"`
		}{timefmt.Time(l.Time), l.Source, l.Stream, l.Line})
	}
	return json.Marshal(struct {
		Time       timefmt.Time `json:"time"`
		Source     string       `json:"source"`
		Stream     *string      `json:"stream"`
		Line       string       `json:"line"`
		Action     string       `json:"action"`
		Program    string       `json:"program"`
		Host       string       `json:"host"`
		Port       int          `json:"port"`
		Credential string       `json:"credential,omitempty"`
	}{timefmt.Time(l.Time), l.Source, nil, l.Line, l.Action, l.Program, l.Host, l.Port, l.Credential})
}

// LogSelection selects the stored lines of a sandbox LogLines reads.
type LogSelection struct {
	// Source keeps the lines of one source; "" keeps both.
	Source string
	// After keeps the lines stored after the line of that Seq; 0 keeps all.
	After int64
	// Limit, when above 0, keeps the first Limit lines.
	Limit int
}

// logColumns are the columns LogLines reads, in the order scanLogLine
// takes them.
const logColumns = `seq, time, source, stream, line, action, program, host, port, credential`

// LogLines reads the stored lines of the sandbox sandboxID that sel
// selects through q, in the order of storage.
func LogLines(ctx context.Context, q access.Querier, sandboxID string, sel LogSelection) ([]LogLine, error) {
	query := `SELECT ` + logColumns + ` FROM sandbox_logs WHERE sandbox_id = ? AND seq > ?`
	args := []any{sandboxID, sel.After}
	if sel.Source != "" {
		query += ` AND source = ?`
		args = append(args, sel.Source)
	}
	query += ` ORDER BY seq`
	if sel.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, sel.Limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the log lines of sandbox %s: %w", sandboxID, err)
	}
	defer rows.Close()
	var out []LogLine
	for rows.Next() {
		var (
			l                                         LogLine
			at                                        int64
			stream, action, program, host, credential sql.NullString
			port                                      sql.NullInt64
		)
		if err := rows.Scan(&l.Seq, &at, &l.Source, &stream, &l.Line, &action, &program, &host, &port, &credential); err != nil {
			return nil, fmt.Errorf("read the log lines of sandbox %s: %w", sandboxID, err)
		}
		l.Time = time.Unix(at, 0).UTC()
		l.Stream, l.Action, l.Program, l.Host, l.Credential = stream.String, action.String, program.String, host.String, credential.String
		l.Port = int(port.Int64)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the log lines of sandbox %s: %w", sandboxID, err)
	}
	return out, nil
}

// insertLogLines stores lines, in their order, as lines of the sandbox
// sandboxID in tx. Their Seq is ignored and their Time cut to whole
// seconds. A policy line stores a NULL stream, a process line NULL policy
// fields, and a policy line without a credential a NULL credential.
func insertLogLines(ctx context.Context, tx *sql.Tx, sandboxID string, lines []LogLine) error {
	for _, l := range lines {
		var stream, action, program, host, credential, port any
		if l.Source == LogSourcePolicy {
			action, program, host, port = l.Action, l.Program, l.Host, l.Port
			if l.Credential != "" {
				credential = l.Credential
			}
		} else {
			stream = l.Stream
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO sandbox_logs (sandbox_id, time, source, stream, line, action, program, host, port, credential)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, sandboxID, l.Time.Unix(), l.Source, stream, l.Line, action, program, host, port, credential)
		if err != nil {
			return fmt.Errorf("store a log line of sandbox %s: %w", sandboxID, err)
		}
	}
	return nil
}

// logResume returns where the reading of the source of the sandbox
// sandboxID's log takes up again: the time of its last stored line, zero
// for none, and how many of its lines of that second are stored.
func logResume(ctx context.Context, q access.Querier, sandboxID, source string) (since time.Time, stored int, err error) {
	var at sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT time, COUNT(*) FROM sandbox_logs WHERE sandbox_id = ? AND source = ? AND time =
		(SELECT MAX(time) FROM sandbox_logs WHERE sandbox_id = ? AND source = ?)`, sandboxID, source, sandboxID, source).Scan(&at, &stored)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("read the last log line of sandbox %s: %w", sandboxID, err)
	}
	if !at.Valid {
		return time.Time{}, 0, nil
	}
	return time.Unix(at.Int64, 0).UTC(), stored, nil
}

// PurgeLogLines deletes the stored lines of the sandbox sandboxID in tx:
// the purge hook the hub registers, run in the transaction that removes the
// sandbox's record.
func PurgeLogLines(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_logs WHERE sandbox_id = ?`, sandboxID); err != nil {
		return fmt.Errorf("delete the log lines of sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// RetainLogs deletes the stored lines more than [sandbox].log_retention
// older than now, and the lines of every sandbox whose record no longer
// exists: the hourly maintenance task of the sandbox logs. Lines of a
// deleted sandbox whose record remains are kept until their time comes.
// With sandboxes off there is no retention, and only the lines without a
// record go.
func (s *Service) RetainLogs(ctx context.Context) error {
	return s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if keep := s.settings.LogRetention; keep > 0 {
			cutoff := s.deps.Now().Add(-keep).Unix()
			if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_logs WHERE time < ?`, cutoff); err != nil {
				return fmt.Errorf("delete the log lines past their retention: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sandbox_logs WHERE NOT EXISTS (SELECT 1 FROM sandboxes WHERE sandboxes.id = sandbox_logs.sandbox_id)`)
		if err != nil {
			return fmt.Errorf("delete the log lines of removed sandboxes: %w", err)
		}
		return nil
	})
}
