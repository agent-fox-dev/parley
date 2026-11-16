package policy_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/policy"
)

func TestFiles_TakesBothPathsOfARenameEachSelectingItsRule(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	commit(t, w.api, "main", "main", map[string]string{"docs/guide.md": "the guide\n"})
	commit(t, w.api, "move", "main", nil, "R docs/guide.md billing/guide.md")
	n := w.open(t, "api", "move").Number

	files, err := w.policy.Files(ctx, w.Store.Main(), "api", n)
	if want := []string{"billing/guide.md", "docs/guide.md"}; err != nil || !reflect.DeepEqual(files, want) {
		t.Fatalf("Files = %q, %v; want %q", files, err, want)
	}
	doc, err := validate(t, `{"rules": [{"name": "docs", "paths": ["docs/**"], "require": {}}, {"name": "billing", "paths": ["billing/**"], "require": {}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	facts := policy.ChangeFacts{Workspace: "api", Number: n, Version: 1, State: change.StateOpen, TargetBranch: "main"}
	res, err := policy.Evaluate(ctx, w.Store.Main(), doc, facts, w.policy, policy.NoEvidence)
	if err != nil {
		t.Fatal(err)
	}
	var got [][]string
	for _, r := range res.Requirements {
		got = append(got, append([]string{r.Rule}, r.Paths...))
	}
	if want := [][]string{{"docs", "docs/guide.md"}, {"billing", "billing/guide.md"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("requirements by rule and paths = %q, want %q", got, want)
	}
}

func TestFiles_AreTheFilesBetweenTheBaseAndTheHeadOfTheCurrentVersion(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	commit(t, w.api, "a", w.base, map[string]string{"billing/a.go": "a\n"})
	below := w.open(t, "api", "a")
	commit(t, w.api, "b", "a", map[string]string{"docs/b.md": "b\n"})
	above := w.open(t, "api", "b", func(in *change.OpenInput) { in.DependsOn = &below.Number })
	if base := w.readChange(t, "api", above.Number).BaseSHA; base == nil || *base != below.HeadSHA {
		t.Fatalf("the base of the change above is %v, want the head of the change below, %s", base, below.HeadSHA)
	}
	generated := map[string]string{}
	for i := range 2100 {
		generated[fmt.Sprintf("gen/f%04d.txt", i)] = fmt.Sprintf("%d\n", i)
	}
	commit(t, w.api, "c", w.base, generated)
	large := w.open(t, "api", "c")

	for _, c := range []struct {
		name   string
		number int64
		want   []string
		count  int
	}{
		{"a change on its target", below.Number, []string{"billing/a.go"}, 1},
		{"a change stacked on another", above.Number, []string{"docs/b.md"}, 1},
		{"a change past the paths a change lists", large.Number, nil, 2100},
	} {
		t.Run(c.name, func(t *testing.T) {
			files, err := w.policy.Files(ctx, w.Store.Main(), "api", c.number)
			if err != nil || len(files) != c.count || (c.want != nil && !reflect.DeepEqual(files, c.want)) {
				t.Errorf("Files = %d files, the first %q, %v; want %d %q", len(files), files[:min(3, len(files))], err, c.count, c.want)
			}
		})
	}
}
