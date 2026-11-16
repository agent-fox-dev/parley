package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

// newChangeOverlapsCommand builds parley change overlaps: the other draft
// and open changes into the same target branch that touch a file the
// change touches, largest overlap first.
func newChangeOverlapsCommand() *cobra.Command {
	return withNumbers(scmRead{
		command: command{
			Name:        "overlaps",
			Description: "List the other draft and open changes into the same target branch that touch a file of a change, largest overlap first",
			Path:        changePath + "/overlaps",
			Args:        []argSpec{slugArg, numberArg},
			FailOn:      overlapFailOn("exit 3 when another change touches a file of the change", changeOverlaps),
		},
	}.build(), true)
}

// changeOverlaps is the condition of parley change overlaps
// --fail-on-overlap: the answer's items hold an entry.
func changeOverlaps(result []byte) (bool, string) {
	var answer struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(result, &answer) != nil || len(answer.Items) == 0 {
		return false, ""
	}
	return true, "another draft or open change touches a file of the change"
}
