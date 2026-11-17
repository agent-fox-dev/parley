package gitrepotest

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// writeServerInfo writes, as git update-server-info does, the files git's
// file-based HTTP transport reads in the repository whose git directory is
// dir: info/refs, every ref with the object it points at, and
// objects/info/packs, every pack. ok is false, with nothing written, for a
// repository whose refs readRefs does not read and for one with tags,
// whose annotated ones need a peeled line it cannot tell without reading
// the objects; the caller runs git then.
func writeServerInfo(dir string) (ok bool) {
	refs, ok := readRefs(dir, "")
	if !ok {
		return false
	}
	var info strings.Builder
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		if strings.HasPrefix(name, "refs/tags/") {
			return false
		}
		fmt.Fprintf(&info, "%s\t%s\n", refs[name], name)
	}
	packDir := filepath.Join(dir, "objects", "pack")
	names, err := filepath.Glob(filepath.Join(packDir, "pack-*.pack"))
	if err != nil {
		return false
	}
	var packs strings.Builder
	for _, name := range names {
		// A pack git is still writing has no index yet, and git lists only
		// the packs it can read.
		if _, err := os.Stat(strings.TrimSuffix(name, ".pack") + ".idx"); err != nil {
			continue
		}
		fmt.Fprintf(&packs, "P %s\n", filepath.Base(name))
	}
	packs.WriteString("\n")
	return replaceFile(filepath.Join(dir, "info", "refs"), info.String()) &&
		replaceFile(filepath.Join(dir, "objects", "info", "packs"), packs.String())
}

// replaceFile replaces the file path with one holding content, through a
// rename, so that a fetch that reads it meanwhile reads the old file or the
// new one whole.
func replaceFile(path, content string) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gitrepotest-*")
	if err != nil {
		return false
	}
	_, err = tmp.WriteString(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return false
	}
	return true
}

// Ref is the object id the ref named in full, such as refs/heads/main,
// points at in the repository whose git directory is dir, "" when there is
// no such ref. It reads the ref files itself where it can rather than start
// git (see readRefs); it needs no test, so a goroutine of one may call it.
func Ref(dir, ref string) string {
	if refs, ok := readRefs(dir, ""); ok {
		return refs[ref]
	}
	id, err := Run(dir, "", "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return id
}
