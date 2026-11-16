package change

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// readResolutions reads the resolutions member of the body of a manual
// restack, by path, nil when there is none: a list of objects, each with a
// path and exactly one of content (the file's text), content_base64 (its
// bytes in standard base64) and delete, which must be true. A path is
// relative to the root of the repository: not empty, with no leading "/"
// and no empty, "." or ".." segment, and named by one entry only. Any
// other list is 400 invalid_request, refused as a whole before anything is
// replayed.
func readResolutions(body map[string]json.RawMessage) (map[string]gitrepo.Resolution, error) {
	list, err := member[[]json.RawMessage](body, "resolutions", "a list of resolutions")
	if err != nil || list == nil {
		return nil, err
	}
	out := make(map[string]gitrepo.Resolution, len(*list))
	for i, raw := range *list {
		name := fmt.Sprintf("resolutions[%d]", i)
		var entry map[string]json.RawMessage
		if isNull(raw) || json.Unmarshal(raw, &entry) != nil {
			return nil, server.InvalidRequest(name + ": must be an object")
		}
		var path string
		if raw, ok := entry["path"]; ok && (isNull(raw) || json.Unmarshal(raw, &path) != nil) {
			return nil, server.InvalidRequest(name + ".path: must be a string")
		}
		switch {
		case path == "":
			return nil, server.InvalidRequest(name + ".path: must not be empty")
		case !relativePath(path):
			return nil, server.InvalidRequest(name + ".path: must be relative, with no empty, \".\" or \"..\" segment")
		}
		if _, dup := out[path]; dup {
			return nil, server.InvalidRequest(fmt.Sprintf("%s.path: %s is named by another resolution", name, path))
		}
		res, err := readResolution(entry)
		if err != nil {
			return nil, server.InvalidRequest(name + "." + err.Error())
		}
		out[path] = res
	}
	return out, nil
}

// readResolution is the resolution of an entry of resolutions whose path
// was read: what its one member of content, content_base64 and delete
// says, or the error that names what is wrong with the members.
func readResolution(entry map[string]json.RawMessage) (gitrepo.Resolution, error) {
	given := 0
	for _, m := range []string{"content", "content_base64", "delete"} {
		if _, ok := entry[m]; ok {
			given++
		}
	}
	if given != 1 {
		return gitrepo.Resolution{}, errors.New("must have exactly one of content, content_base64 and delete")
	}
	if content, err := member[string](entry, "content", "a string"); err != nil || content != nil {
		if err != nil {
			return gitrepo.Resolution{}, errors.New("content: must be a string")
		}
		return gitrepo.Resolution{Content: []byte(*content)}, nil
	}
	if encoded, err := member[string](entry, "content_base64", "a string"); err != nil || encoded != nil {
		var content []byte
		// The standard decoder skips line breaks and lets padding bits be
		// set, so that one content would have several encodings.
		if err == nil && strings.ContainsAny(*encoded, "\r\n") {
			err = errors.New("line break")
		}
		if err == nil {
			content, err = base64.StdEncoding.Strict().DecodeString(*encoded)
		}
		if err != nil {
			return gitrepo.Resolution{}, errors.New("content_base64: must be a string in standard base64")
		}
		return gitrepo.Resolution{Content: content}, nil
	}
	del, err := member[bool](entry, "delete", "true")
	if err != nil || !*del {
		return gitrepo.Resolution{}, errors.New("delete: must be true")
	}
	return gitrepo.Resolution{Delete: true}, nil
}

// relativePath reports whether path names a file below the root of a
// repository: it does not start with "/", and none of its segments is
// empty, "." or "..".
func relativePath(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
