// Package policy decides what may land by path. A workspace owner states
// once, as a document of ordered rules, what each part of the repository
// needs before a change touching it may land: nothing, named checks, a
// person's approval, or a block. The package validates such a document and
// keeps it in one normal form, so that what is stored is always a document
// the evaluation can apply and a refused one names the rule and member to
// fix.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Require is what a rule, or the default, demands of a change: a number of
// approvals from the approvers, named checks (on the current version only
// when FreshChecks is set), or a block.
type Require struct {
	Approvals   int      `json:"approvals"`
	Approvers   []string `json:"approvers"`
	Checks      []string `json:"checks"`
	FreshChecks bool     `json:"fresh_checks"`
	Block       bool     `json:"block"`
}

// Rule is one rule of a policy document: the files its Paths match and its
// Exclude does not, on the target branches its Targets match (every branch
// when Targets is nil), are governed by Require.
type Rule struct {
	Name    string   `json:"name"`
	Paths   []string `json:"paths"`
	Exclude []string `json:"exclude"`
	Targets []string `json:"targets,omitempty"`
	Require Require  `json:"require"`
}

// Document is a policy document in its normal form: Default always present,
// every requirement member filled, every Exclude a list, and Targets kept
// only where the author gave it. Its JSON encoding validates to an equal
// Document.
type Document struct {
	Default Require `json:"default"`
	Rules   []Rule  `json:"rules"`
}

// NeverSet is the document of a workspace that never set a policy, revision
// 0, and of every task fork: no rules, and a default that requires nothing,
// so every change may land. Callers must not modify it.
var NeverSet = Document{
	Default: Require{Approvers: []string{"owner"}, Checks: []string{}},
	Rules:   []Rule{},
}

// The limits of a document. The size is the byte length of the JSON text as
// sent, the only size a caller can know beforehand.
const (
	maxDocumentBytes = 65536
	maxRules         = 200
	maxGlobRunes     = 255
	maxApprovals     = 10
)

var (
	ruleName  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	checkName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
)

// defaultRule is the name the default requirement is reported under, and
// so a name no rule may take.
const defaultRule = "default"

// InvalidPolicyError is a document that breaks a rule of the format. It
// names the first violation only: Rule is the rule's name ("default" for
// the default requirement, nil for the document as a whole or a rule
// without a usable name), Index the rule's zero-based position (nil outside
// a rule), and Field the member at fault, prefixed "require." inside a
// rule's requirement and followed by "[i]" when one entry of a list is.
type InvalidPolicyError struct {
	Rule   *string
	Index  *int
	Field  string
	Reason string
}

func (e *InvalidPolicyError) Error() string {
	where := "document"
	switch {
	case e.Rule != nil && e.Index != nil:
		where = fmt.Sprintf("rule %q (index %d)", *e.Rule, *e.Index)
	case e.Rule != nil:
		where = fmt.Sprintf("rule %q", *e.Rule)
	case e.Index != nil:
		where = fmt.Sprintf("rule at index %d", *e.Index)
	}
	return fmt.Sprintf("invalid policy: %s, field %s: %s", where, e.Field, e.Reason)
}

// APIError is the error the REST API answers for e: 400 invalid_policy with
// details {rule, index, field}, absent ones written as null, so that a
// client finds the violation without parsing the message.
func (e *InvalidPolicyError) APIError() *server.Error {
	return server.NewError("invalid_policy", 400, e.Error()).WithDetails(struct {
		Rule  *string `json:"rule"`
		Index *int    `json:"index"`
		Field string  `json:"field"`
	}{e.Rule, e.Index, e.Field})
}

// Validate checks the JSON text raw against the format of a policy document
// and returns its normal form. A document that breaks the format is refused
// with an *InvalidPolicyError naming the first violation, in a fixed order:
// size, top-level member types, rule count, default, then each rule in
// document order. A failure of the user lookup is returned as it is.
func Validate(ctx context.Context, raw []byte, users Users) (*Document, error) {
	if len(raw) > maxDocumentBytes {
		return nil, invalid(nil, nil, "document", fmt.Sprintf("is longer than %d bytes", maxDocumentBytes))
	}
	top, ok := decodeObject(raw)
	if !ok {
		return nil, invalid(nil, nil, "document", "is not a JSON object")
	}
	var list []any
	if v, present := top["rules"]; present {
		if list, ok = v.([]any); !ok {
			return nil, invalid(nil, nil, "rules", "is not a list")
		}
	}
	var def map[string]any
	if v, present := top["default"]; present {
		if def, ok = v.(map[string]any); !ok {
			return nil, invalid(new(defaultRule), nil, "default", "is not an object")
		}
	}
	if len(list) > maxRules {
		return nil, invalid(nil, nil, "rules", fmt.Sprintf("holds more than %d rules", maxRules))
	}

	v := validator{ctx: ctx, users: users}
	doc := &Document{Rules: make([]Rule, 0, len(list))}
	var err error
	if doc.Default, err = v.require(def, new(defaultRule), nil, ""); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(list))
	for i, item := range list {
		r, err := v.rule(item, i, seen)
		if err != nil {
			return nil, err
		}
		doc.Rules = append(doc.Rules, r)
	}
	return doc, nil
}

// decodeObject decodes raw as one JSON object, numbers kept as written so
// that an integer is told apart from a fraction.
func decodeObject(raw []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func invalid(rule *string, index *int, field, reason string) *InvalidPolicyError {
	return &InvalidPolicyError{Rule: rule, Index: index, Field: field, Reason: reason}
}

// validator holds what checking a requirement needs beyond the document.
type validator struct {
	ctx   context.Context
	users Users
}

// rule checks the rule at index i, in the order name, paths, exclude,
// targets, require. seen holds the names of the earlier rules and gets this
// one's.
func (v validator) rule(item any, i int, seen map[string]bool) (Rule, error) {
	obj, ok := item.(map[string]any)
	if !ok {
		return Rule{}, invalid(nil, &i, fmt.Sprintf("rules[%d]", i), "is not an object")
	}
	name, ok := obj["name"].(string)
	if !ok {
		return Rule{}, invalid(nil, &i, "name", "is missing or not a string")
	}
	fail := func(field, reason string) (Rule, error) {
		return Rule{}, invalid(&name, &i, field, reason)
	}
	switch {
	case !ruleName.MatchString(name):
		return fail("name", "must match "+ruleName.String())
	case name == defaultRule:
		return fail("name", `"default" is the name of the default requirement`)
	case seen[name]:
		return fail("name", "an earlier rule has the same name")
	}
	seen[name] = true

	r := Rule{Name: name}
	var field, reason string
	if r.Paths, field, reason = globs(obj, "paths", true); field != "" {
		return fail(field, reason)
	}
	if r.Exclude, field, reason = globs(obj, "exclude", false); field != "" {
		return fail(field, reason)
	}
	if r.Exclude == nil {
		r.Exclude = []string{}
	}
	if r.Targets, field, reason = targets(obj); field != "" {
		return fail(field, reason)
	}
	req, present := obj["require"]
	reqObj, ok := req.(map[string]any)
	if !present || !ok {
		return fail("require", "is missing or not an object")
	}
	var err error
	if r.Require, err = v.require(reqObj, &name, &i, "require."); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// globs reads the path-glob list member of obj: each entry 1 to 255
// characters, not starting with /. An absent member is nil, or a violation
// when required. A violation is returned as its field and reason.
func globs(obj map[string]any, member string, required bool) ([]string, string, string) {
	entries, present, field, reason := stringList(obj, member)
	switch {
	case field != "":
		return nil, field, reason
	case !present && required:
		return nil, member, "is missing"
	case present && required && len(entries) == 0:
		return nil, member, "is empty"
	}
	for i, g := range entries {
		if n := utf8.RuneCountInString(g); n < 1 || n > maxGlobRunes || strings.HasPrefix(g, "/") {
			return nil, entry(member, i), fmt.Sprintf("must be 1 to %d characters and not start with /", maxGlobRunes)
		}
	}
	return entries, "", ""
}

// targets reads the branch globs of a rule, checked by the hub's one
// validator of branch globs so that a target means what a protected branch
// glob means.
func targets(obj map[string]any) ([]string, string, string) {
	entries, present, field, reason := stringList(obj, "targets")
	switch {
	case field != "":
		return nil, field, reason
	case !present:
		return nil, "", ""
	case len(entries) == 0:
		return nil, "targets", "is empty"
	}
	for i, g := range entries {
		if err := workspace.ValidateBranchGlob(g); err != nil {
			return nil, entry("targets", i), err.Error()
		}
	}
	return entries, "", ""
}

// stringList reads member of obj as a list of strings; present is false when
// the member is absent. A member that is not a list, or an entry that is not
// a string, is returned as a violation.
func stringList(obj map[string]any, member string) (entries []string, present bool, field, reason string) {
	v, present := obj[member]
	if !present {
		return nil, false, "", ""
	}
	list, ok := v.([]any)
	if !ok {
		return nil, true, member, "is not a list"
	}
	entries = make([]string, len(list))
	for i, item := range list {
		if entries[i], ok = item.(string); !ok {
			return nil, true, entry(member, i), "is not a string"
		}
	}
	return entries, true, "", ""
}

func entry(member string, i int) string { return fmt.Sprintf("%s[%d]", member, i) }

// require checks a requirement, the default (obj nil when absent) or a
// rule's, in the order approvals, approvers, checks, fresh_checks, block;
// prefix is put before each field it names.
func (v validator) require(obj map[string]any, rule *string, index *int, prefix string) (Require, error) {
	fail := func(field, reason string) (Require, error) {
		return Require{}, invalid(rule, index, prefix+field, reason)
	}
	r := Require{Approvers: []string{"owner"}, Checks: []string{}}

	if raw, present := obj["approvals"]; present {
		n, ok := raw.(json.Number)
		count, err := strconv.Atoi(string(n))
		if !ok || err != nil || count < 0 || count > maxApprovals {
			return fail("approvals", fmt.Sprintf("must be an integer from 0 to %d", maxApprovals))
		}
		r.Approvals = count
	}

	approvers, present, field, reason := stringList(obj, "approvers")
	switch {
	case field != "":
		return fail(field, reason)
	case present && len(approvers) == 0:
		return fail("approvers", "is empty")
	case present:
		r.Approvers = approvers
	}
	for i, a := range approvers {
		ok, err := v.approver(a)
		if err != nil {
			return Require{}, err
		}
		if !ok {
			return fail(entry("approvers", i), `must be "owner", "member" or "user:" followed by an existing username`)
		}
	}

	checks, _, field, reason := stringList(obj, "checks")
	if field != "" {
		return fail(field, reason)
	}
	for i, c := range checks {
		if !checkName.MatchString(c) {
			return fail(entry("checks", i), "must match "+checkName.String())
		}
	}
	if checks != nil {
		r.Checks = checks
	}

	for _, flag := range []struct {
		member string
		to     *bool
	}{{"fresh_checks", &r.FreshChecks}, {"block", &r.Block}} {
		if raw, present := obj[flag.member]; present {
			b, ok := raw.(bool)
			if !ok {
				return fail(flag.member, "is not a boolean")
			}
			*flag.to = b
		}
	}
	return r, nil
}

// approver reports whether a is an approver the format knows. A user entry
// is valid when the username exists, whatever the user's status, since a
// later change of status does not make the text wrong.
func (v validator) approver(a string) (bool, error) {
	switch a {
	case "owner", "member":
		return true, nil
	}
	username, ok := strings.CutPrefix(a, "user:")
	if !ok || username == "" {
		return false, nil
	}
	exists, err := v.users.UserExists(v.ctx, username)
	if err != nil {
		return false, fmt.Errorf("look up approver %q: %w", username, err)
	}
	return exists, nil
}
