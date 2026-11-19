package gitrepo

import (
	"slices"
	"strings"
	"sync"
)

// The hub compares the same two commits of a repository several times: a
// change version's files are measured when it opens, and read again by the
// awareness worker and by the policy check. A comparison of two object ids
// does not change while git reads the repository's history as stored, so
// DiffChanges keeps its recent answers here and gives one again, without a
// git process, as long as the repository still holds both sides.

const (
	// maxCachedDiffs bounds the answers kept, the oldest going first, and
	// maxCachedChanges the file changes of one answer kept.
	maxCachedDiffs   = 256
	maxCachedChanges = 256
	// emptyTreeID is the id of the empty tree, which git knows without
	// holding it.
	emptyTreeID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// diffs are the answers DiffChanges keeps.
var diffs = &diffCache{answers: map[string][]FileChange{}}

// diffCache is a bounded map from a comparison to its file changes.
type diffCache struct {
	mu      sync.Mutex
	answers map[string][]FileChange
	order   []string // keys, oldest first
}

// get is a copy of the answer kept for key.
func (c *diffCache) get(key string) ([]FileChange, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changes, ok := c.answers[key]
	return slices.Clone(changes), ok
}

// put keeps a copy of changes as the answer for key.
func (c *diffCache) put(key string, changes []FileChange) {
	if len(changes) > maxCachedChanges {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.answers[key]; ok {
		return
	}
	for len(c.order) >= maxCachedDiffs {
		delete(c.answers, c.order[0])
		c.order = c.order[1:]
	}
	c.answers[key] = slices.Clone(changes)
	c.order = append(c.order, key)
}

// diffKey is the key of the comparison of the sides from and to in r, and
// whether its answer may be kept: not when r's environment changes where
// or how git reads the repository's objects.
func (r *Repo) diffKey(from, to string) (string, bool) {
	for _, kv := range r.g.Env {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS":
			return "", false
		}
	}
	return strings.Join(append([]string{r.dir, from, to}, r.g.Env...), "\x00"), true
}

// historyUnchanged is whether an answer kept for the sides from and to in
// r still holds: git reads r's history as stored, and r holds both sides
// (the empty tree needs no object), so that a repository made anew at the
// same path, or one that lost an object, is asked again.
func (r *Repo) historyUnchanged(from, to string) bool {
	if !historyAsStored(r.dir) {
		return false
	}
	for _, id := range []string{from, to} {
		if id == emptyTreeID {
			continue
		}
		if _, ok := readObjectType(r.dir, id); !ok {
			return false
		}
	}
	return true
}
