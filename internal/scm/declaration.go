package scm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"unicode/utf8"

	"github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// maxDeclarationName is the most characters the name of a declaration's
// address may have.
const maxDeclarationName = 200

// declarationKinds are the kinds of declaration the outline reports, the
// values an address's kind may take.
var declarationKinds = map[string]bool{
	string(outline.KindFunc): true, string(outline.KindMethod): true, string(outline.KindType): true,
	string(outline.KindClass): true, string(outline.KindInterface): true, string(outline.KindEnum): true,
	string(outline.KindTrait): true, string(outline.KindConst): true, string(outline.KindVar): true,
	string(outline.KindModule): true, string(outline.KindMacro): true,
}

// address names the declarations of a file: those of the name, and of the
// container and the kind where they are not empty, each compared exactly.
type address struct {
	Name, Container, Kind string
}

// check is why the address cannot name a declaration, "" when it can.
func (a address) check() string {
	switch n := utf8.RuneCountInString(a.Name); {
	case n == 0:
		return "name is required"
	case n > maxDeclarationName:
		return fmt.Sprintf("name has %d characters, more than the %d a declaration's name may have", n, maxDeclarationName)
	case a.Kind != "" && !declarationKinds[a.Kind]:
		return fmt.Sprintf("kind %q is none of func, method, type, class, interface, enum, trait, const, var, module and macro", a.Kind)
	}
	return ""
}

// matches reports whether the address names the declaration d.
func (a address) matches(d outline.Decl) bool {
	return d.Name == a.Name && (a.Container == "" || d.Container == a.Container) && (a.Kind == "" || string(d.Kind) == a.Kind)
}

// find is the declarations of decls the address names, in their order.
func (a address) find(decls []outline.Decl) []outline.Decl {
	var found []outline.Decl
	for _, d := range decls {
		if a.matches(d) {
			found = append(found, d)
		}
	}
	return found
}

// declarationAnswer is one declaration of the declaration route's answer:
// the outline's fields and the declaration's lines, null when its last line
// is not known.
type declarationAnswer struct {
	outlineDecl
	Content *string `json:"content"`
}

// declarationsAnswer is the answer of the declaration route.
type declarationsAnswer struct {
	Path         string              `json:"path"`
	ResolvedSHA  string              `json:"resolved_sha"`
	BlobSHA      string              `json:"blob_sha"`
	Language     string              `json:"language"`
	Declarations []declarationAnswer `json:"declarations"`
}

// getDeclaration answers GET …/scm/declaration: the declarations of the
// source file path at ref that the name, the container and the kind of the
// query name, in line order, each with its lines. A file with syntax errors
// is answered as far as its outline goes; a declaration whose last line the
// outline does not know is answered with end_line 0 and no content.
func (c *Component) getDeclaration(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	path, err := pathParam(q, true)
	if err != nil {
		return err
	}
	addr := address{Name: q.Get("name"), Container: q.Get("container"), Kind: q.Get("kind")}
	if why := addr.check(); why != "" {
		return server.InvalidRequest(why)
	}
	rd, err := c.open(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	res, err := rd.resolver.ResolveOptional(ctx, q.Get("ref"))
	if err != nil {
		return err
	}
	if notModified(w, r, res.Full, res.SHA) {
		return nil
	}
	file, err := rd.repo.LookupFile(ctx, res.SHA, path)
	switch {
	case errors.Is(err, gitrepo.ErrNotFound):
		return server.NewError("path_not_found", http.StatusNotFound, fmt.Sprintf("path %q does not exist at %s", path, res.SHA))
	case errors.Is(err, gitrepo.ErrNotFile):
		return server.InvalidRequest(fmt.Sprintf("path %q is a directory or a submodule at %s, not a file", path, res.SHA))
	case err != nil:
		return err
	}
	if file.Mode != modeSymlink && file.Size > maxOutlineSize {
		return tooLarge(path, file.Size)
	}
	var content []byte
	if file.Mode != modeSymlink {
		var buf bytes.Buffer
		if err := rd.repo.WriteBlob(ctx, file.ID, &buf); err != nil {
			return err
		}
		content = buf.Bytes()
	}
	src, refused, err := c.outlineSource(ctx, path, file.Mode, content)
	if err != nil {
		return fmt.Errorf("outline %q at %s: %w", path, res.SHA, err)
	}
	if refused != nil {
		return refused.apiError(nil)
	}
	found := addr.find(src.file.Decls)
	if len(found) == 0 {
		return server.NewError("declaration_not_found", http.StatusNotFound,
			fmt.Sprintf("%s declares nothing named %q at %s", path, addr.Name, res.SHA)+addr.qualifiers())
	}
	answer := declarationsAnswer{Path: path, ResolvedSHA: res.SHA, BlobSHA: file.ID, Language: src.file.Lang,
		Declarations: make([]declarationAnswer, 0, len(found))}
	for _, d := range found {
		a := declarationAnswer{outlineDecl: outlineDeclOf(d)}
		if from, to, ok := lineSpan(content, d.StartLine, d.EndLine); ok {
			text := string(content[from:to])
			a.Content = &text
		} else {
			a.EndLine = 0
		}
		answer.Declarations = append(answer.Declarations, a)
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// qualifiers says, for a message, what the address asks beside the name.
func (a address) qualifiers() string {
	switch {
	case a.Container != "" && a.Kind != "":
		return fmt.Sprintf(" in the container %q of the kind %s", a.Container, a.Kind)
	case a.Container != "":
		return fmt.Sprintf(" in the container %q", a.Container)
	case a.Kind != "":
		return fmt.Sprintf(" of the kind %s", a.Kind)
	}
	return ""
}

// outlineDeclOf is the declaration d as an outline answer gives it.
func outlineDeclOf(d outline.Decl) outlineDecl {
	return outlineDecl{Kind: string(d.Kind), Name: d.Name, Container: d.Container, Signature: d.Signature,
		Exported: d.Exported, StartLine: d.StartLine, EndLine: d.EndLine}
}

// source is a file's bytes and their outline.
type source struct {
	content []byte
	file    outline.File
}

// unaddressable is why a file cannot be addressed by its declarations: its
// error type, status and message, and what the error carries.
type unaddressable struct {
	errorType string
	status    int
	message   string
	// language is the file's language, for unsupported_language.
	language *string
	// size is the file's size in bytes, for payload_too_large.
	size *int64
}

// apiError is the refusal as an API error, with details, when not nil,
// as its details: the language goes into them, and the size beside them as
// the outline route gives it.
func (f *unaddressable) apiError(details map[string]any) *server.Error {
	e := server.NewError(f.errorType, f.status, f.message)
	if f.language != nil {
		if details == nil {
			details = map[string]any{}
		}
		details["language"] = *f.language
	}
	if f.size != nil {
		e = e.With("size", *f.size)
	}
	if details != nil {
		e = e.WithDetails(details)
	}
	return e
}

// tooLarge is the 413 of the file path of size bytes.
func tooLarge(path string, size int64) *server.Error {
	return (&unaddressable{errorType: "payload_too_large", status: http.StatusRequestEntityTooLarge, size: &size,
		message: fmt.Sprintf("file %q is %d bytes, over the outline limit of %d", path, size, maxOutlineSize)}).apiError(nil)
}

// outlineSource outlines content, the bytes of the file path whose mode is
// mode, through the component's bounded parse, or says why the file cannot
// be addressed by its declarations: a symbolic link, whose content is a
// path, and a file this build has no backend for or of no known language
// are unsupported_language; a file over maxOutlineSize is
// payload_too_large; a binary file is binary_file. A file with syntax
// errors is outlined as far as its syntax tree goes: the caller judges
// them.
func (c *Component) outlineSource(ctx context.Context, path, mode string, content []byte) (source, *unaddressable, error) {
	unsupported := func(lang string) *unaddressable {
		return &unaddressable{errorType: "unsupported_language", status: http.StatusUnprocessableEntity, language: &lang,
			message: fmt.Sprintf("file %q is in no language this hub can outline, so it has no declarations to address", path)}
	}
	binary := &unaddressable{errorType: "binary_file", status: http.StatusUnprocessableEntity,
		message: fmt.Sprintf("file %q is binary, and a binary file has no declarations", path)}
	switch size := int64(len(content)); {
	case mode == modeSymlink:
		return source{}, unsupported(""), nil
	case size > maxOutlineSize:
		return source{}, &unaddressable{errorType: "payload_too_large", status: http.StatusRequestEntityTooLarge, size: &size,
			message: fmt.Sprintf("file %q is %d bytes, over the outline limit of %d", path, size, maxOutlineSize)}, nil
	case isBinary(content):
		return source{}, binary, nil
	}
	file, err := c.parse(ctx, "/"+path, content)
	if err != nil {
		return source{}, nil, err
	}
	switch file.NotParsed {
	case "":
	case outline.NotParsedBinary:
		return source{}, binary, nil
	case outline.NotParsedUnsupported:
		return source{}, unsupported(file.Lang), nil
	default:
		return source{}, nil, fmt.Errorf("the outline did not parse the file: %s", file.NotParsed)
	}
	return source{content: content, file: file}, nil, nil
}

// lineSpan is the byte range [from, to) of the lines start to end of src,
// 1-based and inclusive, with the line ending of the last of them; ok is
// false when they are not lines of src.
func lineSpan(src []byte, start, end int) (from, to int, ok bool) {
	if start < 1 || end < start {
		return 0, 0, false
	}
	line, i := 1, 0
	for line < start {
		n := bytes.IndexByte(src[i:], '\n')
		if n < 0 {
			return 0, 0, false
		}
		i += n + 1
		line++
	}
	if i == len(src) {
		return 0, 0, false
	}
	from = i
	for {
		n := bytes.IndexByte(src[i:], '\n')
		if n < 0 {
			if line == end {
				return from, len(src), true
			}
			return 0, 0, false
		}
		i += n + 1
		if line == end {
			return from, i, true
		}
		line++
	}
}

// spliceLines is src with its bytes [from, to), whole lines or the empty
// range between two, replaced by content, which is ended by the file's own
// line ending (lineEnding) when it is not empty and has none. Content put
// after a last line that has no line ending starts on a line of its own.
func spliceLines(src []byte, from, to int, content []byte) []byte {
	eol := lineEnding(src)
	out := make([]byte, 0, len(src)-(to-from)+len(content)+2*len(eol))
	out = append(out, src[:from]...)
	if len(content) > 0 && from > 0 && src[from-1] != '\n' {
		out = append(out, eol...)
	}
	out = append(out, content...)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		out = append(out, eol...)
	}
	return append(out, src[to:]...)
}

// lineEnding is the line ending of the first line of src, CRLF or LF, and
// LF for a file with no line ending.
func lineEnding(src []byte) []byte {
	if i := bytes.IndexByte(src, '\n'); i > 0 && src[i-1] == '\r' {
		return []byte("\r\n")
	}
	return []byte("\n")
}

// The ops of the declaration operations of a commit request.
const (
	opReplaceDeclaration = "replace_declaration"
	opDeleteDeclaration  = "delete_declaration"
	opInsertDeclaration  = "insert_declaration"
)

// maxDeclarationOperations is the most declaration operations one commit
// request may hold, among its maxOperations.
const maxDeclarationOperations = 100

// maxCandidates is the most declarations an ambiguous address's refusal
// lists.
const maxCandidates = 20

// plannedOperation is one operation of a commit request once its shape is
// checked: a file operation as the tree builder takes it, or, when decl is
// set, a declaration operation, which becomes a put of the file it changes
// once the file is read.
type plannedOperation struct {
	op   gitrepo.Operation
	decl *declarationOperation
}

// declarationOperation is a replacement, a deletion or an insertion of a
// file's lines, placed by an address.
type declarationOperation struct {
	op      string
	path    string
	addr    address
	content []byte
	// after places an insertion after the declaration addr names rather
	// than before it.
	after bool
}

// plan is o checked, or why it is malformed.
func (o fileOperation) plan() (plannedOperation, error) {
	op := ""
	if o.Op != nil {
		op = *o.Op
	}
	switch op {
	case opReplaceDeclaration, opDeleteDeclaration, opInsertDeclaration:
	default:
		fileOp, err := o.operation()
		return plannedOperation{op: fileOp}, err
	}
	d := &declarationOperation{op: op, path: str(o.Path)}
	if !gitrepo.ValidPath(d.path) {
		return plannedOperation{}, fmt.Errorf("path %q is missing, absolute, or has an empty, ., .. or .git segment (in any form a file system reads as .git)", d.path)
	}
	if o.ContentBase64 != nil {
		return plannedOperation{}, fmt.Errorf("a %s takes its text as content, not content_base64", op)
	}
	if op != opDeleteDeclaration {
		if o.Content == nil {
			return plannedOperation{}, fmt.Errorf("a %s takes content", op)
		}
		d.content = []byte(*o.Content)
	}
	if op == opInsertDeclaration {
		if (o.After == nil) == (o.Before == nil) {
			return plannedOperation{}, errors.New("an insert_declaration takes exactly one of after and before")
		}
		at := o.Before
		if o.After != nil {
			at, d.after = o.After, true
		}
		d.addr = address{Name: str(at.Name), Container: str(at.Container), Kind: str(at.Kind)}
	} else {
		d.addr = address{Name: str(o.Name), Container: str(o.Container), Kind: str(o.Kind)}
	}
	if why := d.addr.check(); why != "" {
		return plannedOperation{}, errors.New(why)
	}
	return plannedOperation{decl: d}, nil
}

// str is the string p points at, "" for nil.
func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// stagedSource is a file as the operations of a commit request so far have
// left it: its bytes and mode once read or written, or the path of the
// parent's file they are still to be read from.
type stagedSource struct {
	gone    bool
	loaded  bool
	from    string
	content []byte
	mode    string
}

// resolveDeclarations is the operations of a commit request on top of the
// commit parent, as the tree builder takes them: every file operation as it
// is, and every declaration operation as a put of its file's new bytes,
// with the file's mode. Each declaration operation reads its file as the
// operations before it left it, a put's content or a move's source among
// them, and the parent's file otherwise.
//
// A declaration operation on a path that is no file there is 400
// invalid_operation. Then, each naming the operation's index and path: a
// file that cannot be judged is refused before its address is (422
// unsupported_language, 422 binary_file, 413 payload_too_large, 422
// unparseable_file for a file with a syntax error); an address that names
// no declaration is 404 declaration_not_found and one that names several
// 409 ambiguous_declaration; a result over maxOutlineSize is 413
// payload_too_large and one with a syntax error 422 syntax_error.
//
// The reads and parses of all the operations share one read timeout, the
// parses taking their places among those the hub runs at once; a request
// whose work outlasts it is 503 scm_timeout. Nothing has been written to a
// ref when any of these is answered.
func (c *Component) resolveDeclarations(ctx context.Context, git *gitrepo.Repo, parent string, planned []plannedOperation) ([]gitrepo.Operation, []int, error) {
	if !slices.ContainsFunc(planned, func(p plannedOperation) bool { return p.decl != nil }) {
		ops, index := make([]gitrepo.Operation, len(planned)), make([]int, len(planned))
		for i, p := range planned {
			ops[i], index[i] = p.op, i
		}
		return ops, index, nil
	}
	ctx, cancel := context.WithTimeoutCause(ctx, c.deps.ReadTimeout, errReadTimeout)
	defer cancel()
	ops, index, err := c.applyDeclarations(ctx, git, parent, planned)
	var apiErr *server.Error
	if err != nil && !errors.As(err, &apiErr) && context.Cause(ctx) == errReadTimeout {
		return nil, nil, server.NewError("scm_timeout", http.StatusServiceUnavailable,
			fmt.Sprintf("the declaration operations did not finish within the read timeout of %s", c.deps.ReadTimeout))
	}
	return ops, index, err
}

// applyDeclarations is resolveDeclarations within its deadline. It also
// returns, for each operation it returns, the index of the request's
// operation that it stands for.
//
// The put of a declaration operation takes the place of the put of an
// earlier one on the same file when no operation between them touches the
// file, so that a request of many operations on one file holds one result
// of it rather than one per operation.
//
// A declaration operation reads the files as the file operations before it
// would leave them if they apply; when it is refused, those operations are
// applied first, and one that does not apply is the answer instead, so the
// request is refused for its first operation that fails, as without
// declaration operations.
func (c *Component) applyDeclarations(ctx context.Context, git *gitrepo.Repo, parent string, planned []plannedOperation) ([]gitrepo.Operation, []int, error) {
	ops, index := make([]gitrepo.Operation, 0, len(planned)), make([]int, 0, len(planned))
	staged := map[string]*stagedSource{}
	// lastPut is the position in ops of the put of the last declaration
	// operation on a file, while no operation after it touched the file.
	lastPut := map[string]int{}
	current := func(path string) *stagedSource {
		if f, ok := staged[path]; ok {
			return f
		}
		return &stagedSource{from: path}
	}
	refuse := func(err error) error {
		if _, applyErr := git.ApplyOperations(ctx, parent, ops); applyErr != nil {
			var opErr *gitrepo.OperationError
			if errors.As(applyErr, &opErr) {
				return operationRefusal(opErr, index)
			}
		}
		return err
	}
	for i, p := range planned {
		if p.decl == nil {
			op := p.op
			switch op.Op {
			case gitrepo.OpPut:
				mode := op.Mode
				if mode == "" {
					mode = gitrepo.ModeFile
				}
				staged[op.Path] = &stagedSource{loaded: true, content: op.Content, mode: mode}
			case gitrepo.OpDelete:
				staged[op.Path] = &stagedSource{gone: true}
			case gitrepo.OpChmod:
				f := *current(op.Path)
				f.mode = op.Mode
				staged[op.Path] = &f
			case gitrepo.OpMove:
				f := *current(op.From)
				staged[op.From] = &stagedSource{gone: true}
				staged[op.To] = &f
			}
			for _, path := range []string{op.Path, op.From, op.To} {
				delete(lastPut, path)
			}
			ops, index = append(ops, op), append(index, i)
			continue
		}
		d := p.decl
		f := current(d.path)
		if err := c.load(ctx, git, parent, f); err != nil {
			if errors.Is(err, errNoFile) {
				return nil, nil, refuse(server.NewError("invalid_operation", http.StatusBadRequest,
					fmt.Sprintf("operation %d: %s is no file to change by its declarations", i, d.path)).
					WithDetails(map[string]any{"index": i}))
			}
			return nil, nil, err
		}
		content, err := c.applyDeclaration(ctx, i, d, f)
		if err != nil {
			var apiErr *server.Error
			if errors.As(err, &apiErr) {
				return nil, nil, refuse(err)
			}
			return nil, nil, err
		}
		staged[d.path] = &stagedSource{loaded: true, content: content, mode: f.mode}
		put := gitrepo.Operation{Op: gitrepo.OpPut, Path: d.path, Content: content, Mode: f.mode}
		if k, ok := lastPut[d.path]; ok {
			ops[k], index[k] = put, i
			continue
		}
		lastPut[d.path] = len(ops)
		ops, index = append(ops, put), append(index, i)
	}
	return ops, index, nil
}

// errNoFile is load's report of a path that is no file.
var errNoFile = errors.New("no file")

// load reads the bytes and the mode of f from the commit parent when they
// are not read yet, or returns errNoFile for a file that is gone or a path
// that is no file at parent.
func (c *Component) load(ctx context.Context, git *gitrepo.Repo, parent string, f *stagedSource) error {
	if f.gone {
		return errNoFile
	}
	if f.loaded {
		return nil
	}
	entry, err := git.LookupFile(ctx, parent, f.from)
	if errors.Is(err, gitrepo.ErrNotFound) || errors.Is(err, gitrepo.ErrNotFile) {
		return errNoFile
	}
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := git.WriteBlob(ctx, entry.ID, &buf); err != nil {
		return err
	}
	if f.mode == "" {
		f.mode = entry.Mode
	}
	f.content, f.loaded = buf.Bytes(), true
	return nil
}

// applyDeclaration is the bytes of the file f once the declaration
// operation d, the request's operation index, has changed them.
func (c *Component) applyDeclaration(ctx context.Context, index int, d *declarationOperation, f *stagedSource) ([]byte, error) {
	where := map[string]any{"index": index, "path": d.path}
	src, refused, err := c.outlineSource(ctx, d.path, f.mode, f.content)
	if err != nil {
		return nil, fmt.Errorf("outline %q: %w", d.path, err)
	}
	if refused != nil {
		return nil, refused.apiError(where)
	}
	if errs := src.file.SyntaxErrors; len(errs) > 0 {
		where["line"] = errs[0].Line
		return nil, server.NewError("unparseable_file", http.StatusUnprocessableEntity,
			fmt.Sprintf("operation %d: %s has a syntax error on line %d, so its declarations cannot be addressed; write it whole with a put", index, d.path, errs[0].Line)).
			WithDetails(where)
	}
	found := d.addr.find(src.file.Decls)
	switch {
	case len(found) == 0:
		return nil, server.NewError("declaration_not_found", http.StatusNotFound,
			fmt.Sprintf("operation %d: %s declares nothing named %q", index, d.path, d.addr.Name)+d.addr.qualifiers()).WithDetails(where)
	case len(found) > 1:
		candidates := make([]map[string]any, 0, min(len(found), maxCandidates))
		for _, decl := range found[:min(len(found), maxCandidates)] {
			candidates = append(candidates, map[string]any{"kind": string(decl.Kind), "name": decl.Name, "container": decl.Container,
				"signature": decl.Signature, "start_line": decl.StartLine, "end_line": decl.EndLine})
		}
		where["candidates"] = candidates
		return nil, server.NewError("ambiguous_declaration", http.StatusConflict,
			fmt.Sprintf("operation %d: %s declares %d declarations named %q%s; give a container or a kind to name one", index, d.path, len(found), d.addr.Name, d.addr.qualifiers())).
			WithDetails(where)
	}
	from, to, ok := lineSpan(f.content, found[0].StartLine, found[0].EndLine)
	if !ok {
		return nil, fmt.Errorf("operation %d: the outline of %s gives %q the lines %d to %d, which the file does not have",
			index, d.path, d.addr.Name, found[0].StartLine, found[0].EndLine)
	}
	var result []byte
	switch {
	case d.op == opDeleteDeclaration:
		result = spliceLines(f.content, from, to, nil)
	case d.op == opInsertDeclaration && d.after:
		result = spliceLines(f.content, to, to, d.content)
	case d.op == opInsertDeclaration:
		result = spliceLines(f.content, from, from, d.content)
	default:
		result = spliceLines(f.content, from, to, d.content)
	}
	return result, c.checkResult(ctx, index, d.path, f.mode, result)
}

// checkResult refuses result, the bytes the declaration operation index
// leaves in the file path whose mode is mode, when it is over
// maxOutlineSize or does not parse.
func (c *Component) checkResult(ctx context.Context, index int, path, mode string, result []byte) error {
	where := map[string]any{"index": index, "path": path}
	src, refused, err := c.outlineSource(ctx, path, mode, result)
	if err != nil {
		return fmt.Errorf("outline %q: %w", path, err)
	}
	if refused != nil {
		return refused.apiError(where)
	}
	if len(src.file.SyntaxErrors) == 0 {
		return nil
	}
	errs := make([]map[string]any, len(src.file.SyntaxErrors))
	for i, e := range src.file.SyntaxErrors {
		errs[i] = map[string]any{"line": e.Line, "column": e.Column}
	}
	where["errors"] = errs
	first := src.file.SyntaxErrors[0]
	return server.NewError("syntax_error", http.StatusUnprocessableEntity,
		fmt.Sprintf("operation %d: %s would have a syntax error at line %d, column %d", index, path, first.Line, first.Column)).
		WithDetails(where)
}

// operationRefusal is the 400 invalid_operation of opErr, an operation the
// tree builder could not apply, named by the index of the request's
// operation it stands for (index maps the one to the other).
func operationRefusal(opErr *gitrepo.OperationError, index []int) *server.Error {
	i := index[opErr.Index]
	message := fmt.Sprintf("operation %d: %v", i, opErr.Err)
	if opErr.Path != "" {
		message = fmt.Sprintf("operation %d: %s: %v", i, opErr.Path, opErr.Err)
	}
	return server.NewError("invalid_operation", http.StatusBadRequest, message).WithDetails(map[string]any{"index": i})
}
