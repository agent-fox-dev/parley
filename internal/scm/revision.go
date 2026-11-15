package scm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Repository is what the reads ask of a workspace's repository;
// *gitrepo.Repo is the one the hub opens.
type Repository interface {
	// ListRefs lists the refs whose names begin with one of prefixes, every
	// ref with none.
	ListRefs(ctx context.Context, prefixes ...string) ([]gitrepo.Ref, error)
	// PeelCommit is the commit a full object id reaches through annotated
	// tags.
	PeelCommit(ctx context.Context, id string) (string, error)
	// ResolveCommit is the one commit a full or abbreviated id names.
	ResolveCommit(ctx context.Context, hex string) (string, error)
	// History is the commits reachable from the commit from, newest first,
	// that q keeps.
	History(ctx context.Context, from string, q gitrepo.HistoryQuery) ([]gitrepo.Commit, error)
	// ReadCommits reads the commits of full ids, in their order.
	ReadCommits(ctx context.Context, ids []string) ([]gitrepo.Commit, error)
	// CommitStats are each commit's numbers against its first parent.
	CommitStats(ctx context.Context, commits []gitrepo.Commit) ([]gitrepo.DiffStats, error)
	// DiffChanges are the file changes between two full ids, from "" being
	// the empty tree.
	DiffChanges(ctx context.Context, from, to string) ([]gitrepo.FileChange, error)
	// DiffPatches reads the patch of each file change DiffChanges lists for
	// the same ids, in its order, until each returns false.
	DiffPatches(ctx context.Context, from, to string, opt gitrepo.PatchOptions, each func(i int, patch []byte) bool) error
	// MergeBase is the newest common ancestor of the commits of two full
	// ids, "" when they have none.
	MergeBase(ctx context.Context, a, b string) (string, error)
	// AheadBehind counts the commits only head reaches (ahead) and only
	// base reaches (behind), of two full ids.
	AheadBehind(ctx context.Context, base, head string) (ahead, behind int, err error)
	// CommitsBetween lists the commits head reaches and base does not, of
	// two full ids, oldest first; with limit above 0, the first limit.
	CommitsBetween(ctx context.Context, base, head string, limit int) ([]string, error)
	// TrialRebase replays the commits of head not in onto on top of onto,
	// two full ids, changing no ref, and reports the first that does not
	// apply, nil when all do; a merge commit among them is
	// gitrepo.ErrMergeCommit.
	TrialRebase(ctx context.Context, onto, head string) (*gitrepo.ConflictReport, error)
	// ReadTree reads the directory path, "" for the root, of the commit of
	// a full id, with recursive every entry below it.
	ReadTree(ctx context.Context, commit, path string, recursive bool) (gitrepo.Tree, error)
	// LookupFile is the entry of the file path of the commit of a full id.
	LookupFile(ctx context.Context, commit, path string) (gitrepo.TreeEntry, error)
	// WriteBlob writes the bytes of the blob of a full id to w.
	WriteBlob(ctx context.Context, id string, w io.Writer) error
	// Blame groups the lines first to last, 0 being the first and the last
	// line, of the file path of the commit of a full id by the commit that
	// last changed them, following renames, in line order.
	Blame(ctx context.Context, commit, path string, first, last int) ([]gitrepo.BlameRange, error)
	// Grep searches the text files of the commit of a full id for the lines
	// q matches, in path then line order, until each returns false; a
	// pattern git cannot take is gitrepo.ErrInvalidPattern.
	Grep(ctx context.Context, commit string, q gitrepo.GrepQuery, each func(gitrepo.GrepMatch) bool) error
}

// Ref is a ref of a workspace as its ref view shows it.
type Ref struct {
	// Name is the full ref name, such as refs/heads/main.
	Name string
	// SHA is the object the ref points at: the tag object of an annotated
	// tag.
	SHA string
	// Commit is the commit SHA reaches, when the view knows it; "" makes
	// the resolver peel SHA through the repository.
	Commit string
	// Tracking is set on a fork's tracking branch, which shows its parent's
	// branch of the same name.
	Tracking bool
}

// RefView answers the refs of one workspace. A request reads it once.
type RefView interface {
	Refs(ctx context.Context) ([]Ref, error)
}

// RepoRefs is the ref view of a workspace that is what its repository
// holds: every ref of repo, with the commit each reaches.
func RepoRefs(repo Repository) RefView { return repoRefs{repo} }

type repoRefs struct{ repo Repository }

func (v repoRefs) Refs(ctx context.Context) ([]Ref, error) {
	listed, err := v.repo.ListRefs(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, len(listed))
	for i, r := range listed {
		refs[i] = Ref{Name: r.Name, SHA: r.ID, Commit: r.Commit}
	}
	return refs, nil
}

// Resolved is a revision resolved to a commit.
type Resolved struct {
	// SHA is the commit's full 40-character id.
	SHA string
	// Full is set when the caller wrote the revision as that full id, so
	// that the answer cannot change.
	Full bool
}

// Resolver resolves the revisions of one request against one workspace. It
// reads the workspace's ref view at most once, when a revision first needs
// it, so that every revision of the request is resolved against the same
// refs. It is not safe for concurrent use.
type Resolver struct {
	repo          Repository
	view          RefView
	defaultBranch string
	refs          map[string]Ref
}

// NewResolver is the resolver of a request on the workspace whose
// repository is repo, whose refs view shows and whose default branch, from
// its record, is defaultBranch.
func NewResolver(repo Repository, view RefView, defaultBranch string) *Resolver {
	return &Resolver{repo: repo, view: view, defaultBranch: defaultBranch}
}

// Resolve resolves rev to one commit. In order: a rev that begins with
// refs/ is a full ref name and nothing else; otherwise the branch or the
// tag of that name, and both existing is ambiguous; otherwise 4 to 40
// hexadecimal characters are a commit id, which names a commit whether or
// not a ref reaches it. Any other string, such as main~2, HEAD or --all, is
// no revision. Every failure to name exactly one commit is the API error
// ref_not_found (404); any other error is the repository's.
func (r *Resolver) Resolve(ctx context.Context, rev string) (Resolved, error) {
	if strings.HasPrefix(rev, "refs/") {
		return r.ref(ctx, rev, rev)
	}
	if rev != "" {
		if err := r.load(ctx); err != nil {
			return Resolved{}, err
		}
		_, isBranch := r.refs["refs/heads/"+rev]
		_, isTag := r.refs["refs/tags/"+rev]
		switch {
		case isBranch && isTag:
			return Resolved{}, refNotFound(fmt.Sprintf("revision %q is both a branch and a tag", rev))
		case isBranch:
			return r.ref(ctx, rev, "refs/heads/"+rev)
		case isTag:
			return r.ref(ctx, rev, "refs/tags/"+rev)
		}
	}
	if !isCommitID(rev) {
		return Resolved{}, refNotFound(fmt.Sprintf("revision %q names no commit", rev))
	}
	sha, err := r.repo.ResolveCommit(ctx, rev)
	if err != nil {
		return Resolved{}, notFound(rev, err)
	}
	return Resolved{SHA: sha, Full: len(rev) == commitIDLen}, nil
}

// ResolveOptional is Resolve for a revision a read may leave out: an empty
// rev is the head of the workspace's default branch.
func (r *Resolver) ResolveOptional(ctx context.Context, rev string) (Resolved, error) {
	if rev == "" {
		return r.ref(ctx, "", "refs/heads/"+r.defaultBranch)
	}
	return r.Resolve(ctx, rev)
}

// ref resolves the full ref name name, which rev named, through the view.
func (r *Resolver) ref(ctx context.Context, rev, name string) (Resolved, error) {
	if err := r.load(ctx); err != nil {
		return Resolved{}, err
	}
	ref, found := r.refs[name]
	if !found {
		return Resolved{}, refNotFound(fmt.Sprintf("revision %q names no commit", rev))
	}
	if ref.Commit != "" {
		return Resolved{SHA: ref.Commit}, nil
	}
	sha, err := r.repo.PeelCommit(ctx, ref.SHA)
	if err != nil {
		return Resolved{}, notFound(rev, err)
	}
	return Resolved{SHA: sha}, nil
}

// load reads the ref view, once.
func (r *Resolver) load(ctx context.Context) error {
	if r.refs != nil {
		return nil
	}
	refs, err := r.view.Refs(ctx)
	if err != nil {
		return err
	}
	r.refs = make(map[string]Ref, len(refs))
	for _, ref := range refs {
		r.refs[ref.Name] = ref
	}
	return nil
}

// notFound is err as the API error ref_not_found when it says that rev
// names no one commit, err itself otherwise.
func notFound(rev string, err error) error {
	switch {
	case errors.Is(err, gitrepo.ErrAmbiguous):
		return refNotFound(fmt.Sprintf("revision %q names more than one commit", rev))
	case errors.Is(err, gitrepo.ErrNotCommit):
		return refNotFound(fmt.Sprintf("revision %q names an object that is not a commit", rev))
	case errors.Is(err, gitrepo.ErrNotFound):
		return refNotFound(fmt.Sprintf("revision %q names no commit", rev))
	}
	return err
}

func refNotFound(message string) *server.Error {
	return server.NewError("ref_not_found", http.StatusNotFound, message)
}

// commitIDLen is the length of a full commit id.
const commitIDLen = 40

// isCommitID reports whether s is a full or abbreviated commit id: 4 to 40
// hexadecimal characters.
func isCommitID(s string) bool {
	if len(s) < 4 || len(s) > commitIDLen {
		return false
	}
	return strings.Trim(s, "0123456789abcdefABCDEF") == ""
}
