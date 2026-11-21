package codeindex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/agentkit-go/codesearch"
	"github.com/agent-fox-dev/agentkit-go/tools"
	akworkspace "github.com/agent-fox-dev/agentkit-go/workspace"
)

// Bounds are what stops a build before it has indexed every file.
type Bounds struct {
	MaxFiles     int
	MaxBytes     int64
	MaxBuildTime time.Duration
}

// Index is one code index over a tree. Build builds it, or loads the index
// a build stored under the same directories before; State describes what it
// holds once built; Close releases it and keeps what Build stored.
type Index interface {
	Build(ctx context.Context) error
	// State is how many files the index holds and whether a bound stopped
	// its build.
	State(ctx context.Context) (files int, partial bool, err error)
	// Search answers query, a query in the index's syntax, with at most
	// maxFiles files, best first, each with its matching lines. A query the
	// index cannot parse is a *QueryError; a search it did not finish in
	// its own time is ErrSearchTimeout.
	Search(ctx context.Context, query string, maxFiles int) (SearchResult, error)
	// Symbols are the declarations that match q, from the outlines the
	// build collected. answered is false when the index does not answer
	// such a lookup: it is partial, or more declarations match than it
	// answers.
	Symbols(ctx context.Context, q SymbolQuery) (symbols []Symbol, answered bool, err error)
	Close() error
}

// SymbolQuery is a lookup of declarations by name: the declarations whose
// name starts with Name, ignoring case unless Name has an upper-case
// letter, or is Name when Exact; of the kind Kind unless it is ""; in the
// files under the directory Dir, slash-separated and relative to the tree,
// unless it is "".
type SymbolQuery struct {
	Name, Kind, Dir string
	Exact           bool
}

// Symbol is a declaration a lookup found.
type Symbol struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// SearchResult is the index's answer to a search.
type SearchResult struct {
	// Files are the files that match, best first.
	Files []SearchFile
	// Truncated says that more files matched than were asked for.
	Truncated bool
	// Partial says that a bound stopped the build of the index.
	Partial bool
}

// SearchFile is one file of a search's answer: its path, slash-separated
// and relative to the tree, and its matching lines in line order.
type SearchFile struct {
	Path  string
	Lines []SearchLine
}

// SearchLine is a line that matches: its number, 1-based, and its text.
type SearchLine struct {
	Line int
	Text string
}

// QueryError is the error of a query the index cannot parse; Message is its
// parser's.
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return "the index cannot parse the query: " + e.Message }

// ErrSearchTimeout is the error of a search the index did not finish in its
// own time.
var ErrSearchTimeout = errors.New("the search did not finish in the index's time: narrow the query or add a path constraint")

// ErrUnsupported is the error of an Opener on a platform the index cannot
// run on.
var ErrUnsupported = codesearch.ErrUnsupported

// Opener makes the index of the files under tree that keeps what it builds
// under store, a directory outside tree, and works in scratch. It builds
// nothing and touches no disk.
type Opener func(tree, store, scratch string, b Bounds) (Index, error)

// OpenSearchIndex is the Opener of the hub: agentkit-go's codesearch index,
// which reads the tree's own ignore files and no other.
func OpenSearchIndex(tree, store, scratch string, b Bounds) (Index, error) {
	ws, err := akworkspace.Open(tree, akworkspace.Options{Ignore: akworkspace.NoGlobalExcludes()})
	if err != nil {
		return nil, err
	}
	idx, err := codesearch.New(ws, codesearch.Options{MaxFiles: b.MaxFiles, MaxBytes: b.MaxBytes, MaxBuildTime: b.MaxBuildTime,
		TempDir: scratch, IndexDir: store})
	if err != nil {
		return nil, err
	}
	built, ok := idx.(*codesearch.Index)
	if !ok {
		_ = idx.Close()
		return nil, fmt.Errorf("the code index is a %T, which has no build", idx)
	}
	return searchIndex{built}, nil
}

// searchIndex is a codesearch index as the component uses it.
type searchIndex struct{ idx *codesearch.Index }

func (s searchIndex) Build(ctx context.Context) error { return s.idx.Build(ctx) }

func (s searchIndex) Close() error { return s.idx.Close() }

// State asks the index's own search tool, the one place the index says
// whether a bound stopped its build, with a query that reads no file.
func (s searchIndex) State(ctx context.Context) (int, bool, error) {
	data, err := s.search(ctx, map[string]any{"query": "file:^$", "max_files": 1})
	if err != nil {
		return 0, false, fmt.Errorf("the index did not answer for its state: %w", err)
	}
	partial, _ := data["partial"].(bool)
	return s.idx.BuildStats().FilesIndexed, partial, nil
}

func (s searchIndex) Search(ctx context.Context, query string, maxFiles int) (SearchResult, error) {
	// No context lines: the answer is the lines that match.
	data, err := s.search(ctx, map[string]any{"query": query, "max_files": maxFiles, "context_lines": 0})
	if err != nil {
		return SearchResult{}, err
	}
	// The tool's result is its own types; its JSON form is its contract.
	raw, err := json.Marshal(data)
	if err != nil {
		return SearchResult{}, err
	}
	var answer struct {
		Files []struct {
			Path   string `json:"path"`
			Chunks []struct {
				Lines []struct {
					Line  int    `json:"line"`
					Text  string `json:"text"`
					Match bool   `json:"match"`
				} `json:"lines"`
			} `json:"chunks"`
		} `json:"files"`
		Truncated bool `json:"truncated"`
		Partial   bool `json:"partial"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return SearchResult{}, fmt.Errorf("read the index's answer: %w", err)
	}
	res := SearchResult{Files: make([]SearchFile, 0, len(answer.Files)), Truncated: answer.Truncated, Partial: answer.Partial}
	for _, f := range answer.Files {
		file := SearchFile{Path: f.Path}
		for _, chunk := range f.Chunks {
			for _, l := range chunk.Lines {
				if l.Match {
					file.Lines = append(file.Lines, SearchLine{Line: l.Line, Text: l.Text})
				}
			}
		}
		// The chunks come best first; the lines are answered in line order.
		slices.SortFunc(file.Lines, func(a, b SearchLine) int { return a.Line - b.Line })
		file.Lines = slices.CompactFunc(file.Lines, func(a, b SearchLine) bool { return a.Line == b.Line })
		res.Files = append(res.Files, file)
	}
	return res, nil
}

func (s searchIndex) Symbols(ctx context.Context, q SymbolQuery) ([]Symbol, bool, error) {
	answer, answered, err := s.idx.Symbols(ctx, tools.SymbolQuery{Name: q.Name, Kind: q.Kind, Path: q.Dir, Exact: q.Exact})
	if err != nil || !answered {
		return nil, false, err
	}
	symbols := make([]Symbol, 0, len(answer.Matches))
	for _, m := range answer.Matches {
		symbols = append(symbols, Symbol{Path: m.Path, Kind: m.Kind, Name: m.Name, Container: m.Container,
			Signature: m.Signature, Exported: m.Exported, StartLine: m.StartLine, EndLine: m.EndLine})
	}
	// The index walks its files in no order; a lookup answers in one.
	slices.SortFunc(symbols, func(a, b Symbol) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), a.StartLine-b.StartLine, strings.Compare(a.Name, b.Name))
	})
	return symbols, true, nil
}

// search runs the index's own search tool with args and returns its data.
// The tool's refusals become the errors of Search: a query it cannot take
// is a *QueryError, a search it timed out ErrSearchTimeout, and one it
// aborted the end of ctx.
func (s searchIndex) search(ctx context.Context, args map[string]any) (map[string]any, error) {
	for _, tool := range s.idx.Tools() {
		if tool.Name != "code_search" || tool.Execute == nil {
			continue
		}
		in, _ := json.Marshal(args)
		res := tool.Execute(ctx, in)
		switch {
		case res.OK:
			return res.Data, nil
		case res.Error == "invalid_arguments":
			return nil, &QueryError{Message: strings.TrimPrefix(res.Detail, "query parse error: ")}
		case res.Error == "search_failed" && strings.Contains(res.Detail, "timed out"):
			return nil, ErrSearchTimeout
		case res.Error == "aborted":
			if err := context.Cause(ctx); err != nil {
				return nil, err
			}
			return nil, context.Canceled
		}
		return nil, fmt.Errorf("%s: %s", res.Error, res.Detail)
	}
	return nil, errors.New("the index has no search tool")
}
