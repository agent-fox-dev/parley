package server

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

type versionDocument struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`

	// extra are the members of Options.VersionMembers, written after the
	// build's in name order.
	extra map[string]any
}

// MarshalJSON writes the build's members first, in their fixed order, then
// the extra members that do not reuse a build member's name.
func (d versionDocument) MarshalJSON() ([]byte, error) {
	type build versionDocument // without the method
	out, err := json.Marshal(build(d))
	if err != nil || len(d.extra) == 0 {
		return out, err
	}
	var b bytes.Buffer
	b.Write(out[:len(out)-1]) // without the closing brace
	for _, k := range slices.Sorted(maps.Keys(d.extra)) {
		switch k {
		case "version", "commit", "build_time", "go_version":
			continue
		}
		name, _ := json.Marshal(k)
		value, err := json.Marshal(d.extra[k])
		if err != nil {
			return nil, err
		}
		b.WriteByte(',')
		b.Write(name)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", versionCacheControl)
	info := buildinfo.Get()
	doc := versionDocument{
		Version:   info.Version,
		Commit:    info.Commit,
		BuildTime: info.BuildTime,
		GoVersion: info.GoVersion,
	}
	if s.opts.VersionMembers != nil {
		doc.extra = s.opts.VersionMembers()
	}
	WriteJSON(w, http.StatusOK, doc)
}
