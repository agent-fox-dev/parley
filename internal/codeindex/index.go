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
	for _, tool := range s.idx.Tools() {
		if tool.Name != "code_search" || tool.Execute == nil {
			continue
		}
		in, _ := json.Marshal(map[string]any{"query": "file:^$", "max_files": 1})
		res := tool.Execute(ctx, in)
		if !res.OK {
			return 0, false, fmt.Errorf("the index did not answer for its state: %s: %s", res.Error, res.Detail)
		}
		partial, _ := res.Data["partial"].(bool)
		return s.idx.BuildStats().FilesIndexed, partial, nil
	}
	return 0, false, errors.New("the index has no search tool to ask for its state")
}
