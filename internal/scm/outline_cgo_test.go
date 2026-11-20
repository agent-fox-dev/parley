//go:build cgo

package scm_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Only a hub built with cgo has a backend for a language other than Go, so
// this file is left out of a build without it.
func TestOutline_OutlinesEveryLanguageOfTheList(t *testing.T) {
	t.Parallel()
	samples := []struct{ path, language, declares, content string }{
		{"a.go", "Go", "greet", "package a\n\nfunc greet() {}\n"},
		{"a.py", "Python", "greet", "def greet():\n    return 1\n"},
		{"a.js", "JavaScript", "greet", "function greet() { return 1; }\n"},
		{"a.ts", "TypeScript", "greet", "function greet(): number { return 1; }\n"},
		{"a.rs", "Rust", "greet", "pub fn greet() -> i32 { 1 }\n"},
		{"A.java", "Java", "Greeter", "public class Greeter {\n    void hi() {}\n}\n"},
		{"a.kt", "Kotlin", "greet", "fun greet(): Int = 1\n"},
		{"A.cs", "C#", "Greeter", "public class Greeter {\n    void Hi() {}\n}\n"},
		{"a.rb", "Ruby", "greet", "def greet\n  1\nend\n"},
		{"a.c", "C", "greet", "int greet(void) { return 1; }\n"},
		{"a.cpp", "C++", "greet", "int greet() { return 1; }\n"},
		{"a.php", "PHP", "greet", "<?php\nfunction greet() { return 1; }\n"},
		{"a.scala", "Scala", "Greeter", "object Greeter {\n  def hi(): Int = 1\n}\n"},
		{"a.lua", "Lua", "greet", "function greet()\n  return 1\nend\n"},
		{"a.sh", "Shell", "greet", "greet() {\n  echo hi\n}\n"},
	}
	var stream strings.Builder
	stream.WriteString("commit refs/heads/main\n" + testCommitter + "data 5\nlangs\n")
	for _, s := range samples {
		stream.WriteString("M 100644 inline " + s.path + "\ndata " + strconv.Itoa(len(s.content)) + "\n" + s.content + "\n")
	}
	w := newWorld(t)
	w.addWorkspace(t, "langs", w.org, stream.String()+"\n")

	for _, s := range samples {
		t.Run(s.language, func(t *testing.T) {
			r, a := outlineOf(t, w, "langs", "path="+s.path)
			if r.status != http.StatusOK || !a.is(true, s.language) {
				t.Fatalf("%s: %d %s, want 200 with supported true and language %s", s.path, r.status, r.body, s.language)
			}
			i := slices.IndexFunc(a.Declarations, func(d declaration) bool { return d.Name == s.declares })
			if i < 0 || a.Declarations[i].StartLine < 1 {
				t.Errorf("%s: declarations %+v, want one named %s with a start_line of 1 or more", s.path, a.Declarations, s.declares)
			}
		})
	}
}
