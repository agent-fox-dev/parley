package scm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/server"
)

// opCommit is the op a commit request reports.
const opCommit = "commit"

// commitsPattern is the pattern of the commit route below
// /workspaces/:slug/scm.
const commitsPattern = "/commits"

// maxOperations is the most file operations one commit request may hold.
const maxOperations = 1000

// DefaultMaxCommitSize is the body limit of the commit route, in bytes, when
// LimitCommitBody sets none.
const DefaultMaxCommitSize = 32 << 20

// LimitCommitBody sets the body limit of the commit route among routes,
// such as those Routes returned, to limit bytes, in place of the server's
// body limit; zero or less leaves it as it is. A commit carries the whole
// content of the files it writes, so its body has a limit of its own.
func LimitCommitBody(routes []server.Route, limit int64) {
	if limit <= 0 {
		return
	}
	for i, r := range routes {
		if r.Method == http.MethodPost && r.Pattern == "/workspaces/:slug/scm"+commitsPattern {
			routes[i].MaxBodySize = limit
		}
	}
}

// commitRequest is the body of POST …/scm/commits.
type commitRequest struct {
	Branch          *string         `json:"branch"`
	BaseRef         *string         `json:"base_ref"`
	ExpectedHeadSHA *string         `json:"expected_head_sha"`
	Message         *string         `json:"message"`
	Author          *commitAuthor   `json:"author"`
	Operations      []fileOperation `json:"operations"`
	AllowEmpty      bool            `json:"allow_empty"`
}

// commitAuthor is the author a commit request names.
type commitAuthor struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// fileOperation is one file operation of a commit request.
type fileOperation struct {
	Op            *string `json:"op"`
	Path          *string `json:"path"`
	From          *string `json:"from"`
	To            *string `json:"to"`
	Content       *string `json:"content"`
	ContentBase64 *string `json:"content_base64"`
	Mode          *string `json:"mode"`
}

// commitAnswer is the answer to a commit request: the new commit, with its
// numbers, the branch at it, and the change it made a version of or opened
// and why opening one failed, both null when there is none.
type commitAnswer struct {
	Commit      commitObject `json:"commit"`
	Branch      refObject    `json:"branch"`
	Change      any          `json:"change"`
	ChangeError any          `json:"change_error"`
}

// createCommit answers POST …/scm/commits: one commit made from the
// request's file operations, on top of the head of the branch it names, or
// of the commit base_ref names for a branch that does not exist, and the
// branch moved to it, or created there, through the shared ref update step.
//
// The checks run in the order in which each has what it needs: the shape of
// the body and of every operation (400 invalid_request), before anything is
// read from the repository; the parent (409 ref_moved for an existing branch
// whose expected_head_sha is absent or another commit, so that one answer
// tells the caller what to retry from; 400 invalid_request for a new branch
// without base_ref; 404 ref_not_found when base_ref names no commit); the
// operations (400 invalid_operation, naming the first that does not apply);
// a commit that changes nothing (400 empty_commit unless allow_empty); then
// the push rules and the compare-and-swap against the parent. The objects
// written for a request that is then refused stay unreferenced, for git's
// garbage collection: no ref moves and no event is emitted.
func (c *Component) createCommit(w http.ResponseWriter, r *http.Request, wr *write) error {
	var body commitRequest
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	branch, ops, err := body.check()
	if err != nil {
		return err
	}
	// The change object, refused on a protected branch, and the session are
	// checked here, before the parent is resolved.

	ctx := r.Context()
	t, err := c.target(ctx, wr)
	if err != nil {
		return err
	}
	parent, before, err := t.parent(ctx, branch, body)
	if err != nil {
		return err
	}
	tree, err := t.git.ApplyOperations(ctx, parent, ops)
	if err != nil {
		var opErr *gitrepo.OperationError
		if errors.As(err, &opErr) {
			return server.NewError("invalid_operation", http.StatusBadRequest, opErr.Error()).
				WithDetails(map[string]any{"index": opErr.Index})
		}
		return err
	}
	parents, err := t.git.ReadCommits(ctx, []string{parent})
	if err != nil {
		return err
	}
	if tree == parents[0].Tree && !body.AllowEmpty {
		return server.NewError("empty_commit", http.StatusBadRequest,
			"the operations leave the tree as it is; set allow_empty to commit it anyway")
	}
	now := c.deps.Now()
	sha, err := t.git.CreateCommit(ctx, gitrepo.CommitSpec{Tree: tree, Parents: []string{parent},
		Message: *body.Message, Author: authorOf(wr, body.Author, now), Committer: gitrepo.HubSignature(now)})
	if err != nil {
		return err
	}
	if _, err := c.updateRef(ctx, wr, t.git, refUpdate{ref: branch, before: before, after: sha,
		versionKind: pushrule.VersionKindCommit}); err != nil {
		return err
	}
	return c.writeCommit(w, r, t, branch, sha)
}

// check is the full name of the branch the request names and its
// operations as the tree builder takes them, or 400 invalid_request for a
// body or an operation of the wrong shape.
func (b *commitRequest) check() (string, []gitrepo.Operation, error) {
	if b.Branch == nil {
		return "", nil, server.InvalidRequest("branch is required")
	}
	branch, err := branchName(*b.Branch)
	if err != nil {
		return "", nil, err
	}
	if b.Message == nil || *b.Message == "" {
		return "", nil, server.InvalidRequest("message is required and must not be empty")
	}
	if b.ExpectedHeadSHA != nil && !isFullID(*b.ExpectedHeadSHA) {
		return "", nil, server.InvalidRequest("expected_head_sha must be a full 40-character commit id")
	}
	if a := b.Author; a != nil && (!signaturePart(a.Name) || !signaturePart(a.Email)) {
		return "", nil, server.InvalidRequest("author needs a name and an email, neither holding <, > or a line break")
	}
	if len(b.Operations) > maxOperations {
		return "", nil, server.InvalidRequest(fmt.Sprintf("a commit takes at most %d operations, not %d", maxOperations, len(b.Operations)))
	}
	ops := make([]gitrepo.Operation, len(b.Operations))
	for i, o := range b.Operations {
		if ops[i], err = o.operation(); err != nil {
			return "", nil, server.InvalidRequest(fmt.Sprintf("operation %d: %v", i, err))
		}
	}
	return branch, ops, nil
}

// branchName is the full ref name of the branch name, given as its name or
// its full name below refs/heads/, or 400 invalid_request for a name git
// does not take for a branch.
func branchName(name string) (string, error) {
	full := name
	if !strings.HasPrefix(name, "refs/") {
		full = "refs/heads/" + name
	}
	if name == "" || name == "HEAD" || !strings.HasPrefix(full, "refs/heads/") || !gitrepo.ValidRefName(full) {
		return "", server.InvalidRequest(fmt.Sprintf("branch %q is not a valid branch name", name))
	}
	return full, nil
}

// signaturePart reports whether s can be the name or the e-mail address of
// a commit's author: present, not blank, and free of what ends one in a
// commit object.
func signaturePart(s *string) bool {
	return s != nil && strings.TrimSpace(*s) != "" && !strings.ContainsAny(*s, "<>\n\x00")
}

// operation is o as the tree builder takes it, or why it is malformed.
func (o fileOperation) operation() (gitrepo.Operation, error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	op := gitrepo.Operation{Op: str(o.Op), Path: str(o.Path), From: str(o.From), To: str(o.To), Mode: str(o.Mode)}
	paths := []string{op.Path}
	switch op.Op {
	case gitrepo.OpPut:
		if (o.Content == nil) == (o.ContentBase64 == nil) {
			return op, errors.New("a put takes exactly one of content and content_base64")
		}
		if o.Content != nil {
			op.Content = []byte(*o.Content)
		} else {
			b, err := base64.StdEncoding.DecodeString(*o.ContentBase64)
			if err != nil {
				return op, errors.New("content_base64 is not valid base64")
			}
			op.Content = b
		}
		if o.Mode != nil && op.Mode != gitrepo.ModeFile && op.Mode != gitrepo.ModeExecutable && op.Mode != gitrepo.ModeSymlink {
			return op, fmt.Errorf("a put takes the mode %s, %s or %s, not %q", gitrepo.ModeFile, gitrepo.ModeExecutable, gitrepo.ModeSymlink, op.Mode)
		}
	case gitrepo.OpDelete:
	case gitrepo.OpChmod:
		if op.Mode != gitrepo.ModeFile && op.Mode != gitrepo.ModeExecutable {
			return op, fmt.Errorf("a chmod takes the mode %s or %s", gitrepo.ModeFile, gitrepo.ModeExecutable)
		}
	case gitrepo.OpMove:
		paths = []string{op.From, op.To}
	case "":
		return op, errors.New("op is required")
	default:
		return op, fmt.Errorf("op %q is none of put, delete, move and chmod", op.Op)
	}
	for _, p := range paths {
		if !gitrepo.ValidPath(p) {
			return op, fmt.Errorf("path %q is missing, absolute, or has an empty, ., .. or .git segment", p)
		}
	}
	return op, nil
}

// parent is the commit a commit request builds on and the value its branch
// must have when it moves: for an existing branch its head, which
// expected_head_sha must name, base_ref being ignored; for a new branch the
// commit base_ref names, and pushrule.ZeroID.
func (t *target) parent(ctx context.Context, branch string, b commitRequest) (parent, before string, err error) {
	ref, found, err := t.git.LookupRef(ctx, branch)
	if err != nil {
		return "", "", err
	}
	if found {
		head := commitOf(ref)
		if b.ExpectedHeadSHA == nil || strings.ToLower(*b.ExpectedHeadSHA) != head {
			return "", "", refMoved(branch, &head)
		}
		return head, ref.ID, nil
	}
	if b.BaseRef == nil || *b.BaseRef == "" {
		return "", "", server.InvalidRequest(fmt.Sprintf("branch %s does not exist; base_ref names the commit to create it on", branch))
	}
	res, err := t.resolver.Resolve(ctx, *b.BaseRef)
	if err != nil {
		return "", "", err
	}
	return res.SHA, pushrule.ZeroID, nil
}

// authorOf is the author of a commit the write wr makes at the time at: the
// request's author when it names one, else the caller, by full name or else
// username, with the e-mail address, and the hub for the admin token, which
// acts for no user.
func authorOf(wr *write, a *commitAuthor, at time.Time) gitrepo.Signature {
	switch u := wr.principal.User; {
	case a != nil:
		return gitrepo.Signature{Name: *a.Name, Email: *a.Email, Time: at}
	case u != nil:
		name := u.FullName
		if strings.TrimSpace(name) == "" {
			name = u.Username
		}
		return gitrepo.Signature{Name: name, Email: u.Email, Time: at}
	}
	return gitrepo.HubSignature(at)
}

// writeCommit answers 201 with the commit sha, with its numbers, and the
// branch at it.
func (c *Component) writeCommit(w http.ResponseWriter, r *http.Request, t *target, branch, sha string) error {
	ctx := r.Context()
	commits, err := t.git.ReadCommits(ctx, []string{sha})
	if err != nil {
		return err
	}
	stats, err := t.git.CommitStats(ctx, commits)
	if err != nil {
		return err
	}
	objs, err := c.commitObjects(ctx, t.read, commits, stats)
	if err != nil {
		return err
	}
	ref, _, err := c.refObject(r, t.read, Ref{Name: branch, SHA: sha, Commit: sha})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, commitAnswer{Commit: objs[0], Branch: ref})
	return nil
}
