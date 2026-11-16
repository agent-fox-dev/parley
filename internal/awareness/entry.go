package awareness

import (
	"slices"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// maxEntryPaths is the most files in common an overlap entry lists.
const maxEntryPaths = 100

// maxSummaryIntent is the most characters of a change's intent its summary
// carries.
const maxSummaryIntent = 500

// Entry is an overlap entry: the other change's summary, the files the two
// changes have in common, the first 100 of them in ascending byte order,
// whether there were more, and how many there are.
type Entry struct {
	Change         Summary  `json:"change"`
	Paths          []string `json:"paths"`
	PathsTruncated bool     `json:"paths_truncated"`
	Count          int      `json:"count"`
}

// Summary is a change as an overlap entry shows it: enough to know whose
// it is and how far along, with the intent cut to its first 500
// characters and the landing reduced to its status.
type Summary struct {
	Number    int64          `json:"number"`
	Ref       string         `json:"ref"`
	Title     string         `json:"title"`
	Intent    string         `json:"intent"`
	State     string         `json:"state"`
	Author    SummaryAuthor  `json:"author"`
	SessionID *string        `json:"session_id"`
	Version   int64          `json:"version"`
	UpdatedAt timefmt.Time   `json:"updated_at"`
	Mergeable string         `json:"mergeable"`
	Landing   SummaryLanding `json:"landing"`
}

// SummaryAuthor is the author of a summarised change, as the change shows
// it.
type SummaryAuthor struct {
	ActorType string  `json:"actor_type"`
	ActorID   *string `json:"actor_id"`
	Username  *string `json:"username"`
}

// SummaryLanding is the landing of a summarised change: its status alone.
type SummaryLanding struct {
	Status string `json:"status"`
}

// EntryOf is the overlap entry of the change other, which has the files
// common in common with the change the entry is listed for, in any order.
func EntryOf(other Change, common []string) Entry {
	paths := slices.Sorted(slices.Values(common))
	return Entry{
		Change:         SummaryOf(other),
		Paths:          paths[:min(len(paths), maxEntryPaths)],
		PathsTruncated: len(paths) > maxEntryPaths,
		Count:          len(paths),
	}
}

// SummaryOf is the summary of the change ch.
func SummaryOf(ch Change) Summary {
	return Summary{
		Number: ch.Number, Ref: refOf(ch), Title: ch.Title, Intent: cutRunes(ch.Intent, maxSummaryIntent), State: ch.State,
		Author:    SummaryAuthor{ActorType: ch.Author.ActorType, ActorID: ch.Author.ActorID, Username: ch.Author.Username},
		SessionID: ch.SessionID, Version: ch.Version, UpdatedAt: timefmt.Time(ch.UpdatedAt), Mergeable: ch.Mergeable,
		Landing: SummaryLanding{Status: ch.LandingStatus},
	}
}

// refOf is the reference of the change ch, workspace#number, as the
// change reader gives it or, when it gives none, as it is made.
func refOf(ch Change) string {
	if ch.Ref != "" {
		return ch.Ref
	}
	return ch.Workspace + "#" + strconv.FormatInt(ch.Number, 10)
}

// cutRunes is s cut to its first n characters.
func cutRunes(s string, n int) string {
	i := 0
	for at := range s {
		if i == n {
			return s[:at]
		}
		i++
	}
	return s
}
