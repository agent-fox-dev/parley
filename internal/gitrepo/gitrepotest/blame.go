package gitrepotest

import "strconv"

// BlameStream is a git fast-import stream of files to blame, four commits
// on main. c1 adds f.txt ("1" to "3", a line each), g.txt ("x", "y", "z"),
// the empty file empty.txt and before.txt ("b1" to "b4"); c2 adds the lines
// "4" to "6" to f.txt; c3 changes the line 3 of f.txt to "three" and
// deletes "y" from g.txt; c4 renames before.txt to renamed.txt and adds
// "b5" to it. Blamed at main, f.txt is lines 1-2 of c1, 3 of c3 and 4-6 of
// c2; g.txt is two lines of c1, its lines 1 and 3; renamed.txt is lines 1-4
// of c1 as before.txt and line 5 of c4.
func BlameStream() string {
	file := func(path, content string) string {
		return "M 100644 inline " + path + "\ndata " + strconv.Itoa(len(content)) + "\n" + content + "\n"
	}
	commit := func(msg string) string {
		return "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 2\n" + msg + "\n"
	}
	return commit("c1") + file("f.txt", "1\n2\n3\n") + file("g.txt", "x\ny\nz\n") + file("empty.txt", "") +
		file("before.txt", "b1\nb2\nb3\nb4\n") +
		commit("c2") + file("f.txt", "1\n2\n3\n4\n5\n6\n") +
		commit("c3") + file("f.txt", "1\n2\nthree\n4\n5\n6\n") + file("g.txt", "x\nz\n") +
		commit("c4") + "D before.txt\n" + file("renamed.txt", "b1\nb2\nb3\nb4\nb5\n") + "\n"
}
