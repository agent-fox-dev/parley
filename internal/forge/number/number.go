// Package number is the rule an issue or pull request number follows. The
// hub applies it to a route's :number and the command-line client to a
// command's <number> argument; it imports the standard library alone, so
// that the client takes the rule without the hub's code.
package number

import (
	"errors"
	"strconv"
)

// Message is the refusal of a number that breaks the rule.
const Message = "number must be a positive decimal integer"

// ErrInvalid is the refusal of a number that breaks the rule; its text is
// Message.
var ErrInvalid = errors.New(Message)

// Max is the largest issue or pull request number: the forges keep them in
// 32 bits.
const Max = 1<<31 - 1

// Parse parses an issue or pull request number: decimal digits only,
// without a sign or a leading zero, from 1 to Max. Anything else is
// ErrInvalid.
func Parse(s string) (int64, error) {
	if s == "" || s[0] == '0' || len(s) > len(strconv.Itoa(Max)) {
		return 0, ErrInvalid
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrInvalid
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > Max {
		return 0, ErrInvalid
	}
	return n, nil
}
