package gitrepotest

import (
	"fmt"
	"strconv"
	"strings"
)

// LongCommits is the number of commits of the branch long of CompareStream.
const LongCommits = 250

// CompareStream is a git fast-import stream of branches to compare and to
// rebase. main is r0, adding x.txt; mb, adding f.txt ("base"), the binary
// file bin, r.txt, t.txt and the submodule entry sub; and a commit that
// changes f.txt to "main", changes bin, renames r.txt to r1.txt, turns
// t.txt into a symlink and moves sub. trimmed is main and a commit deleting
// f.txt. On top of mb: feature, f1 adding a.txt and f2 adding b.txt; long,
// LongCommits commits each writing l.txt; both-edit, a commit adding e.txt
// and one changing f.txt to "head"; head-deletes, deleting f.txt;
// base-deletes, changing f.txt to "head"; both-binary, changing bin;
// both-rename, renaming r.txt to r2.txt; both-type, changing t.txt; and
// both-submodule, moving sub elsewhere. both-add, on top of r0, adds f.txt
// ("head"). merged, on top of main, merges feature.
func CompareStream() string {
	data := func(s string) string { return "data " + strconv.Itoa(len(s)) + "\n" + s + "\n" }
	file := func(path, content string) string { return "M 100644 inline " + path + "\n" + data(content) }
	sub := func(c byte) string { return "M 160000 " + strings.Repeat(string(c), 40) + " sub\n" }
	commit := func(ref, mark, from, msg string) string {
		s := "commit refs/heads/" + ref + "\n"
		if mark != "" {
			s += "mark :" + mark + "\n"
		}
		s += "committer T <t@example.invalid> 1700000000 +0000\n" + data(msg)
		if from != "" {
			s += "from :" + from + "\n"
		}
		return s
	}
	var lines strings.Builder
	for i := range 12 {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	var s strings.Builder
	s.WriteString(commit("main", "1", "", "r0") + file("x.txt", "x\n") +
		commit("main", "2", "", "mb") + file("f.txt", "base\n") + file("bin", "\x00base") + file("r.txt", lines.String()) +
		file("t.txt", "t\n") + sub('5') +
		commit("main", "3", "", "main") + file("f.txt", "main\n") + file("bin", "\x00main") + "D r.txt\n" +
		file("r1.txt", lines.String()) + "M 120000 inline t.txt\n" + data("x.txt") + sub('6') +
		commit("trimmed", "", "3", "trim") + "D f.txt\n" +
		commit("feature", "4", "2", "f1") + file("a.txt", "a\n") +
		commit("feature", "5", "", "f2") + file("b.txt", "b\n") +
		commit("merged", "", "3", "m") + "merge :5\n" + file("a.txt", "a\n") + file("b.txt", "b\n") +
		commit("both-edit", "", "2", "e1") + file("e.txt", "e\n") +
		commit("both-edit", "", "", "e2") + file("f.txt", "head\n") +
		commit("both-add", "", "1", "add") + file("f.txt", "head\n") +
		commit("head-deletes", "", "2", "delete") + "D f.txt\n" +
		commit("base-deletes", "", "2", "modify") + file("f.txt", "head\n") +
		commit("both-binary", "", "2", "binary") + file("bin", "\x00head") +
		commit("both-rename", "", "2", "rename") + "D r.txt\n" + file("r2.txt", lines.String()) +
		commit("both-type", "", "2", "type") + file("t.txt", "t2\n") +
		commit("both-submodule", "", "2", "submodule") + sub('7'))
	for i := range LongCommits {
		from := ""
		if i == 0 {
			from = "2"
		}
		s.WriteString(commit("long", "", from, "l"+strconv.Itoa(i)) + file("l.txt", strconv.Itoa(i)+"\n"))
	}
	return s.String()
}
