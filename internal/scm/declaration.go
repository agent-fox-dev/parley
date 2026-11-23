package scm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
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
