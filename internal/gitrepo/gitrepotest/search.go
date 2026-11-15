package gitrepotest

import (
	"fmt"
	"strconv"
	"strings"
)

// SearchLong is the one line of long.txt in SearchStream: "needle" and
// 1,994 two-byte characters, 2,000 characters in all.
var SearchLong = "needle" + strings.Repeat("é", 1994)

// SearchStream is a fast-import stream of one commit on main whose files a
// content search reads: api/greeting.go, whose lines 3 and 7 declare the
// exported functions Greeting and Farewell and whose line 4 holds
// "greeting"; docs/a.md, whose line 1 is "# Greeting"; literal.txt, whose
// lines are "a.b(c)*" and "aXb"; blob.bin, binary, holding the bytes of
// Greeting; long.txt, the one line SearchLong; hits.txt, 450 lines "hit 1"
// to "hit 450"; crlf.txt, the line "windows line" ended by CR LF; and
// "dir with space/ü.txt", the line "unicode path".
func SearchStream() string {
	file := func(path, content string) string {
		return "M 100644 inline " + path + "\ndata " + strconv.Itoa(len(content)) + "\n" + content + "\n"
	}
	var hits strings.Builder
	for i := 1; i <= 450; i++ {
		fmt.Fprintf(&hits, "hit %d\n", i)
	}
	return "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 6\nsearch\n" +
		file("api/greeting.go", "package api\n\nfunc Greeting() string {\n\treturn \"greeting\"\n}\n\n"+
			"func Farewell() string { return \"bye\" }\nfunc helper() {}\n") +
		file("docs/a.md", "# Greeting\n\nSay hello.\n") + file("literal.txt", "a.b(c)*\naXb\n") +
		file("blob.bin", "\x00Greeting\n") + file("long.txt", SearchLong+"\n") + file("hits.txt", hits.String()) +
		file("crlf.txt", "windows line\r\n") + file("dir with space/ü.txt", "unicode path\n") + "\n"
}
