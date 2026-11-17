package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
)

// readConfig is what git config --local --get answers for key in the git
// directory dir, read from its configuration file in process: the last
// value the file sets for key, and n, how many times it sets it, 0 for a
// key it does not set. ok is false for what it does not read the way git
// does, and the caller asks git then: a key with a subsection, a file it
// cannot read, one that includes another, has a continued line, a
// carriage return or a line git would refuse, and a value of key that is
// quoted, escaped, holds a tab or is given with no "=".
func readConfig(dir, key string) (value string, n int, ok bool) {
	section, name, cut := strings.Cut(key, ".")
	if !cut || !isConfigName(section, false) || !isConfigName(name, true) {
		return "", 0, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil || strings.ContainsRune(string(raw), '\r') {
		return "", 0, false
	}
	inSection, seenSection := false, false
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimRight(line, "\n")
		if strings.HasSuffix(line, `\`) {
			return "", 0, false
		}
		line = strings.TrimLeft(line, " \t")
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			sec, sub, ok := configHeader(line)
			if !ok || strings.EqualFold(sec, "include") || strings.EqualFold(sec, "includeIf") {
				return "", 0, false
			}
			inSection, seenSection = !sub && strings.EqualFold(sec, section), true
			continue
		}
		i := 0
		for i < len(line) && isConfigNameByte(line[i]) {
			i++
		}
		rest := strings.TrimLeft(line[i:], " \t")
		if !seenSection || !isConfigName(line[:i], true) ||
			(rest != "" && rest[0] != '=' && rest[0] != '#' && rest[0] != ';') {
			return "", 0, false
		}
		if !inSection || !strings.EqualFold(line[:i], name) {
			continue
		}
		v, assigned := strings.CutPrefix(rest, "=")
		if !assigned || strings.ContainsAny(v, "\"\\") {
			return "", 0, false
		}
		if end := strings.IndexAny(v, "#;"); end >= 0 {
			v = v[:end]
		}
		v = strings.Trim(v, " \t")
		if strings.ContainsRune(v, '\t') {
			return "", 0, false
		}
		value, n = v, n+1
	}
	return value, n, true
}

// configHeader reads the section header line, "[section]", "[section.sub]"
// or `[section "sub"]` with nothing but a comment after it: the section's
// name, and whether it names a subsection.
func configHeader(line string) (section string, sub, ok bool) {
	i := 1
	for i < len(line) && (isConfigNameByte(line[i]) || line[i] == '.') {
		i++
	}
	section = line[1:i]
	if section == "" || i == len(line) {
		return "", false, false
	}
	sub = strings.Contains(section, ".")
	if line[i] == ' ' || line[i] == '\t' {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i == len(line) || line[i] != '"' {
			return "", false, false
		}
		for i++; i < len(line) && line[i] != '"'; i++ {
			if line[i] == '\\' {
				i++
			}
		}
		if i >= len(line) {
			return "", false, false
		}
		i++
		sub = true
	}
	if i == len(line) || line[i] != ']' {
		return "", false, false
	}
	after := strings.TrimLeft(line[i+1:], " \t")
	return section, sub, after == "" || after[0] == '#' || after[0] == ';'
}

// isConfigName reports whether s is a section name git takes, letters,
// digits and "-", or, when variable is set, a variable name, which also
// begins with a letter.
func isConfigName(s string, variable bool) bool {
	if s == "" || variable && !('a' <= s[0]|0x20 && s[0]|0x20 <= 'z') {
		return false
	}
	for i := range len(s) {
		if !isConfigNameByte(s[i]) {
			return false
		}
	}
	return true
}

// isConfigNameByte reports whether c may be part of a section or variable
// name.
func isConfigNameByte(c byte) bool {
	return 'a' <= c|0x20 && c|0x20 <= 'z' || '0' <= c && c <= '9' || c == '-'
}
