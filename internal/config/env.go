package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// variableName is the form of NAME in a ${NAME} reference.
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// errMalformedReference is a ${ without a closing }, or a name that is not a
// variable name.
var errMalformedReference = errors.New("invalid environment reference")

// unsetVariableError is a reference to a variable the environment does not
// have.
type unsetVariableError struct{ name string }

func (e unsetVariableError) Error() string {
	return "environment variable " + e.name + " is not set"
}

// expand replaces each ${NAME} in a string value by the variable NAME and
// each $$ by one $; a $ followed by neither { nor $ stays as it is. It runs
// after the file is parsed and only on string values, so a variable's
// content never changes the file's structure. Errors never carry a
// variable's value.
func expand(s string, env LookupEnv) (string, error) {
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
				return "", errMalformedReference
			}
			name := s[i+2 : i+2+end]
			if !variableName.MatchString(name) {
				return "", errMalformedReference
			}
			var v string
			ok := false
			if env != nil {
				v, ok = env(name)
			}
			if !ok {
				return "", unsetVariableError{name: name}
			}
			b.WriteString(v)
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

// expandDeep replaces references in every string value of a parsed section,
// at any depth; path names the value an error is about.
func expandDeep(v any, path string, env LookupEnv) (any, error) {
	switch x := v.(type) {
	case string:
		out, err := expand(x, env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range sortedKeys(x) {
			e, err := expandDeep(x[k], path+"."+k, env)
			if err != nil {
				return nil, err
			}
			out[k] = e
		}
		return out, nil
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, m := range x {
			e, err := expandDeep(m, fmt.Sprintf("%s[%d]", path, i), env)
			if err != nil {
				return nil, err
			}
			out[i] = e.(map[string]any)
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			e, err := expandDeep(item, fmt.Sprintf("%s[%d]", path, i), env)
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	}
	return v, nil
}
