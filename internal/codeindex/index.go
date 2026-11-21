package codeindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/agentkit-go/codesearch"
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
	// Files are the paths of the files whose content matches query, a
	// query of the index, slash-separated and relative to the tree.
	Files(ctx context.Context, query string) ([]string, error)
	Close() error
}

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

func (s searchIndex) Files(ctx context.Context, query string) ([]string, error) {
	data, err := s.search(ctx, map[string]any{"query": query})
	if err != nil {
		return nil, err
	}
	// The tool's result is its own types; its JSON form is its contract.
	raw, err := json.Marshal(data["files"])
	if err != nil {
		return nil, err
	}
	var files []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("read the index's answer: %w", err)
	}
	paths := []string{}
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths, nil
}

// search runs the index's own search tool with args and returns its data.
func (s searchIndex) search(ctx context.Context, args map[string]any) (map[string]any, error) {
	for _, tool := range s.idx.Tools() {
		if tool.Name != "code_search" || tool.Execute == nil {
			continue
		}
		in, _ := json.Marshal(args)
		res := tool.Execute(ctx, in)
		if !res.OK {
			return nil, fmt.Errorf("%s: %s", res.Error, res.Detail)
		}
		return res.Data, nil
	}
	return nil, errors.New("the index has no search tool")
}
