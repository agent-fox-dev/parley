package cli

import (
	"io"
	"os"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The flags that give the text of a write.
const (
	bodyFlag     = "body"
	bodyFileFlag = "body-file"
)

// textInput is the text a command takes from --body or --body-file, sent
// exactly as given or read: a text whose white space or line breaks were
// changed on the way would not be the one the user wrote. It is read while
// the command line is checked, so that a file that cannot be read is a
// usage error before any setting is resolved or any request sent (ADR 02).
type textInput struct {
	// what names the text in the flags' usage, "the issue's body" say.
	what string
	// required makes a command line that gives neither flag a usage error.
	required bool

	// text is the text the command line gives, and given whether it gives
	// one.
	text  string
	given bool
}

// addFlags adds --body and --body-file to fs.
func (in *textInput) addFlags(fs *pflag.FlagSet) {
	fs.String(bodyFlag, "", in.what+", sent as given; the empty string is a text too")
	fs.String(bodyFileFlag, "", "read "+in.what+" from this file, or from standard input for -, and send it exactly as read")
}

// read reads the text the command line gives. Both flags, a file or
// standard input that cannot be read, and a text that is not UTF-8, which a
// JSON body cannot carry unchanged, are usage errors, and so is neither
// flag when the text is required.
func (in *textInput) read(cmd *cobra.Command) error {
	fs := cmd.Flags()
	inline, file := fs.Changed(bodyFlag), fs.Changed(bodyFileFlag)
	switch {
	case inline && file:
		return usageError("%s takes one of --%s and --%s, not both", cmd.CommandPath(), bodyFlag, bodyFileFlag)
	case inline:
		in.text, _ = fs.GetString(bodyFlag)
	case file:
		name, _ := fs.GetString(bodyFileFlag)
		var data []byte
		var err error
		if name == "-" {
			data, err = io.ReadAll(optionsFrom(cmd.Context()).stdin)
		} else {
			data, err = os.ReadFile(name)
		}
		if err != nil {
			return usageError("%s cannot read the text of --%s: %v", cmd.CommandPath(), bodyFileFlag, err)
		}
		in.text = string(data)
	case in.required:
		return usageError("%s needs the flag --%s or --%s", cmd.CommandPath(), bodyFlag, bodyFileFlag)
	default:
		return nil
	}
	if !utf8.ValidString(in.text) {
		return usageError("%s takes UTF-8 text in --%s or --%s; the text given is not", cmd.CommandPath(), bodyFlag, bodyFileFlag)
	}
	in.given = true
	return nil
}
