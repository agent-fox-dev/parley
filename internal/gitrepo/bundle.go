package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// FetchBundle fetches the objects of the git bundle file at path into the
// repository and sets each ref of refs, a full ref name, to the object id
// refs maps it to, all of them in one transaction of git's: they are set
// together or not at all, and a ref that exists is moved.
//
// Git keeps no ref "a/b" beside a ref "a". A ref below the prefix replace
// whose name clashes so with one of refs is deleted first, in a
// transaction of its own, since git refuses both in one; when refs cannot
// then be set, it is put back as it was. An empty replace deletes nothing,
// and such a clash fails the fetch.
//
// Every id must be the tip of a ref of the bundle, so that git's fetch has
// checked that the repository then holds all of its history; a bundle
// whose prerequisite commits the repository lacks fails. The bundle's own
// refs are written below a fresh namespace of the fetch's own, read and
// removed, as FetchOrigin does with origin's (see fetchNamespace), so that
// none of them outlives the fetch.
func (r *Repo) FetchBundle(ctx context.Context, path string, refs map[string]string, replace string) error {
	if len(refs) == 0 {
		return errors.New("fetch a bundle: no ref to set")
	}
	if err := refuseOption("bundle path", path); err != nil {
		return err
	}
	for _, ref := range slices.Sorted(maps.Keys(refs)) {
		if !ValidRefName(ref) {
			return fmt.Errorf("fetch a bundle: %q is not a ref name", ref)
		}
		if err := refuseOptions(ref, refs[ref]); err != nil {
			return err
		}
	}
	ns := fetchNamespace + randomName() + "/"
	tips, err := r.fetchBundle(ctx, path, ns)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	cleanErr := r.deleteUnder(cleanup, ns)
	if err != nil {
		return err
	}
	if cleanErr != nil {
		return fmt.Errorf("remove the fetched refs: %w", cleanErr)
	}
	var script strings.Builder
	for _, ref := range slices.Sorted(maps.Keys(refs)) {
		id := refs[ref]
		if !tips[id] {
			return fmt.Errorf("fetch a bundle: %s is no tip of the bundle", id)
		}
		fmt.Fprintf(&script, "update %s %s\n", ref, id)
	}
	clashing, err := r.clashing(ctx, replace, refs)
	if err != nil {
		return fmt.Errorf("read the refs below %s: %w", replace, err)
	}
	var removal, restore strings.Builder
	for _, ref := range slices.Sorted(maps.Keys(clashing)) {
		fmt.Fprintf(&removal, "delete %s %s\n", ref, clashing[ref])
		fmt.Fprintf(&restore, "create %s %s\n", ref, clashing[ref])
	}
	if err := r.updateRefs(ctx, removal.String()); err != nil {
		return fmt.Errorf("delete the refs that clash with the bundle's: %w", err)
	}
	if err := r.updateRefs(ctx, script.String()); err != nil {
		err = fmt.Errorf("set the refs of the bundle: %w", err)
		// The refs are put back also after ctx ended, as the namespace is
		// removed then.
		back, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if rerr := r.updateRefs(back, restore.String()); rerr != nil {
			err = fmt.Errorf("%w; put back the refs that clashed: %v", err, rerr)
		}
		return err
	}
	return nil
}

// clashing maps each ref below prefix whose name clashes with a ref of refs,
// one naming a directory the other is in, to its object id; none for an
// empty prefix.
func (r *Repo) clashing(ctx context.Context, prefix string, refs map[string]string) (map[string]string, error) {
	if prefix == "" {
		return nil, nil
	}
	have, err := r.refs(ctx, prefix)
	if err != nil {
		return nil, err
	}
	clashing := map[string]string{}
	for name, id := range have {
		name = prefix + name
		for ref := range refs {
			if strings.HasPrefix(ref, name+"/") || strings.HasPrefix(name, ref+"/") {
				clashing[name] = id
				break
			}
		}
	}
	return clashing, nil
}

// fetchBundle fetches every ref of the bundle at path below ns, in one
// atomic fetch, and returns the set of their object ids.
func (r *Repo) fetchBundle(ctx context.Context, path, ns string) (map[string]bool, error) {
	_, err := r.git(ctx, "fetch", "--quiet", "--atomic", "--no-tags", "--no-write-fetch-head",
		"--", path, "+refs/*:"+ns+"*")
	if err != nil {
		return nil, err
	}
	fetched, err := r.refs(ctx, ns)
	if err != nil {
		return nil, err
	}
	tips := map[string]bool{}
	for _, id := range fetched {
		tips[id] = true
	}
	return tips, nil
}

// CreateBundle writes a git bundle of refs, full ref names, with their
// whole history, to the file at path, which it replaces: the bundle needs
// no commit of the repository it is fetched into. A ref the repository
// lacks fails it.
func (r *Repo) CreateBundle(ctx context.Context, path string, refs []string) error {
	if len(refs) == 0 {
		return errors.New("create a bundle: no ref to bundle")
	}
	if err := refuseOption("bundle path", path); err != nil {
		return err
	}
	for _, ref := range refs {
		if err := refuseOption("ref", ref); err != nil {
			return err
		}
		if !ValidRefName(ref) {
			return fmt.Errorf("create a bundle: %q is not a ref name", ref)
		}
	}
	// Git runs in a directory of its own, where a relative path would land.
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("create a bundle: %w", err)
	}
	if _, err := r.git(ctx, append([]string{"bundle", "create", "--quiet", abs}, refs...)...); err != nil {
		return fmt.Errorf("create a bundle: %w", err)
	}
	return nil
}
