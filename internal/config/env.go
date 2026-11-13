package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// varName is the shape of a variable name a ${NAME} reference may hold.
var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// expandTree replaces the environment references in every string value of
// the decoded file, at any depth. It runs on the parsed tree, never on the
// text, so a variable's content cannot add, remove or rename a key or a
// section, and table names, key names, integers and booleans are left alone.
// Errors name the key and the variable, never a variable's value.
func expandTree(tree map[string]any, env Env) error {
	for _, name := range sortedKeys(tree) {
		v, err := expandValue(tree[name], name, env)
		if err != nil {
			return err
		}
		tree[name] = v
	}
	return nil
}

// expandValue returns v with the references in its strings replaced; path is
// the dotted name of v, used in errors.
func expandValue(v any, path string, env Env) (any, error) {
	switch x := v.(type) {
	case string:
		s, err := expand(x, env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return s, nil
	case map[string]any:
		for _, name := range sortedKeys(x) {
			e, err := expandValue(x[name], path+"."+name, env)
			if err != nil {
				return nil, err
			}
			x[name] = e
		}
	case []map[string]any:
		for i, t := range x {
			if _, err := expandValue(t, fmt.Sprintf("%s[%d]", path, i), env); err != nil {
				return nil, err
			}
		}
	case []any:
		for i, e := range x {
			e, err := expandValue(e, fmt.Sprintf("%s[%d]", path, i), env)
			if err != nil {
				return nil, err
			}
			x[i] = e
		}
	}
	return v, nil
}

// expand replaces each ${NAME} in s by the value of the variable NAME and each
// $$ by one $. A $ followed by neither { nor $ stays as it is.
func expand(s string, env Env) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", errors.New("environment reference \"${\" has no closing \"}\"")
			}
			name := s[i+2 : i+2+end]
			if !varName.MatchString(name) {
				return "", fmt.Errorf("environment reference ${%s}: a variable name is a letter or _ followed by letters, digits or _", name)
			}
			value, ok := env(name)
			if !ok {
				return "", fmt.Errorf("environment variable %s is not set", name)
			}
			b.WriteString(value)
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}
