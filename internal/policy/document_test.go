package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/server"
)

// knownUsers answers the validator's user lookup from a fixed set of
// usernames; err, when set, is the lookup's own failure.
type knownUsers struct {
	names map[string]bool
	err   error
}

func (u knownUsers) UserExists(_ context.Context, username string) (bool, error) {
	if u.err != nil {
		return false, u.err
	}
	return u.names[username], nil
}

var users = knownUsers{names: map[string]bool{"dana": true, "blocked-bob": true}}

// threeRules is a document with a rule for docs, one for billing limited to
// some targets, and one that blocks secrets.
const threeRules = `{
  "default": {"approvals": 0},
  "rules": [
    {"name": "docs", "paths": ["docs/**", "**/*.md"], "exclude": ["docs/internal/**"], "require": {}},
    {"name": "billing", "paths": ["billing/**"], "targets": ["main", "release/*"],
     "require": {"approvals": 1, "approvers": ["owner", "user:dana"], "checks": ["test", "billing-e2e"], "fresh_checks": true}},
    {"name": "secrets", "paths": ["deploy/secrets/**"], "require": {"block": true}}
  ]
}`

// owner is the requirement every absent member leaves.
func owner() policy.Require {
	return policy.Require{Approvers: []string{"owner"}, Checks: []string{}}
}

func validate(t *testing.T, raw string) (*policy.Document, error) {
	t.Helper()
	return policy.Validate(context.Background(), []byte(raw), users)
}

func TestValidate_AcceptsADocumentAndReturnsItsNormalForm(t *testing.T) {
	long := strings.Repeat("n", 63)
	glob := strings.Repeat("g", 255)
	cases := []struct {
		name string
		raw  string
		want policy.Document
	}{
		{"empty object", `{}`, policy.Document{Default: owner(), Rules: []policy.Rule{}}},
		{"three rules", threeRules, policy.Document{Default: owner(), Rules: []policy.Rule{
			{Name: "docs", Paths: []string{"docs/**", "**/*.md"}, Exclude: []string{"docs/internal/**"}, Require: owner()},
			{Name: "billing", Paths: []string{"billing/**"}, Exclude: []string{}, Targets: []string{"main", "release/*"},
				Require: policy.Require{Approvals: 1, Approvers: []string{"owner", "user:dana"}, Checks: []string{"test", "billing-e2e"}, FreshChecks: true}},
			{Name: "secrets", Paths: []string{"deploy/secrets/**"}, Exclude: []string{}, Require: policy.Require{Approvers: []string{"owner"}, Checks: []string{}, Block: true}},
		}}},
		{"rule of name, paths and an empty require", `{"rules": [{"name": "a", "paths": ["a/**"], "require": {}}]}`,
			policy.Document{Default: owner(), Rules: []policy.Rule{{Name: "a", Paths: []string{"a/**"}, Exclude: []string{}, Require: owner()}}}},
		{"members the format does not define", `{"comment": "x", "rules": [{"name": "a", "owner_team": "t", "paths": ["a"], "require": {}}]}`,
			policy.Document{Default: owner(), Rules: []policy.Rule{{Name: "a", Paths: []string{"a"}, Exclude: []string{}, Require: owner()}}}},
		{"ten approvals and a name of 63 characters", `{"rules": [{"name": "` + long + `", "paths": ["a"], "require": {"approvals": 10, "approvers": ["member"]}}]}`,
			policy.Document{Default: owner(), Rules: []policy.Rule{{Name: long, Paths: []string{"a"}, Exclude: []string{},
				Require: policy.Require{Approvals: 10, Approvers: []string{"member"}, Checks: []string{}}}}}},
		{"path glob of 255 characters", `{"rules": [{"name": "a", "paths": ["` + glob + `"], "require": {}}]}`,
			policy.Document{Default: owner(), Rules: []policy.Rule{{Name: "a", Paths: []string{glob}, Exclude: []string{}, Require: owner()}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nf, err := validate(t, tc.raw)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !reflect.DeepEqual(*nf, tc.want) {
				t.Fatalf("normal form = %+v, want %+v", *nf, tc.want)
			}
			again, err := validate(t, string(mustMarshal(t, nf)))
			if err != nil || !reflect.DeepEqual(again, nf) {
				t.Fatalf("validating the normal form again = %+v, %v, want %+v", again, err, nf)
			}
		})
	}
}

func TestValidate_NeverSetIsTheNormalFormOfAnEmptyDocument(t *testing.T) {
	nf, err := validate(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*nf, policy.NeverSet) {
		t.Fatalf("NeverSet = %+v, want %+v", policy.NeverSet, *nf)
	}
}

// violation is where a refused document breaks the format.
type violation struct {
	rule  *string
	index *int
	field string
}

func at(rule string, index int, field string) violation { return violation{&rule, &index, field} }

func inDefault(field string) violation { return violation{new("default"), nil, field} }

func wantViolation(t *testing.T, nf *policy.Document, err error, want violation) {
	t.Helper()
	if nf != nil {
		t.Errorf("normal form = %+v, want none", *nf)
	}
	var invalid *policy.InvalidPolicyError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want an invalid-policy error", err)
	}
	got := violation{invalid.Rule, invalid.Index, invalid.Field}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("violation = %s, want %s", show(got), show(want))
	}
}

func show(v violation) string {
	r, i := "null", "null"
	if v.rule != nil {
		r = fmt.Sprintf("%q", *v.rule)
	}
	if v.index != nil {
		i = fmt.Sprint(*v.index)
	}
	return fmt.Sprintf("(%s, %s, %q)", r, i, v.field)
}

// inRule is a document with one rule r whose members are given.
func inRule(members string) string {
	return `{"rules": [{"name": "r", "paths": ["a"], ` + members + `}]}`
}

func TestValidate_RefusesADocumentThatBreaksTheFormat(t *testing.T) {
	type row struct {
		name string
		raw  string
		want violation
	}
	rows := []row{
		{"not an object: list", `[]`, violation{field: "document"}},
		{"not an object: string", `"x"`, violation{field: "document"}},
		{"not an object: null", `null`, violation{field: "document"}},
		{"not JSON", `{"rules": [}`, violation{field: "document"}},
		{"rules an object", `{"rules": {}}`, violation{field: "rules"}},
		{"default a list", `{"default": []}`, inDefault("default")},
		{"rule without name", `{"rules": [{"paths": ["a"], "require": {}}]}`, violation{nil, new(0), "name"}},
		{"name not a string", `{"rules": [{"name": 7, "paths": ["a"], "require": {}}]}`, violation{nil, new(0), "name"}},
		{"name upper case", `{"rules": [{"name": "Docs", "paths": ["a"], "require": {}}]}`, at("Docs", 0, "name")},
		{"name of 64 characters", `{"rules": [{"name": "` + strings.Repeat("a", 64) + `", "paths": ["a"], "require": {}}]}`, at(strings.Repeat("a", 64), 0, "name")},
		{"name starting with -", `{"rules": [{"name": "-x", "paths": ["a"], "require": {}}]}`, at("-x", 0, "name")},
		{"name default", `{"rules": [{"name": "default", "paths": ["a"], "require": {}}]}`, at("default", 0, "name")},
		{"repeated name", `{"rules": [{"name": "docs", "paths": ["a"], "require": {}}, {"name": "docs", "paths": ["b"], "require": {}}]}`, at("docs", 1, "name")},
		{"no paths", `{"rules": [{"name": "r", "require": {}}]}`, at("r", 0, "paths")},
		{"empty paths", `{"rules": [{"name": "r", "paths": [], "require": {}}]}`, at("r", 0, "paths")},
		{"empty path glob", `{"rules": [{"name": "r", "paths": ["ok", ""], "require": {}}]}`, at("r", 0, "paths[1]")},
		{"absolute path glob", `{"rules": [{"name": "r", "paths": ["/abs"], "require": {}}]}`, at("r", 0, "paths[0]")},
		{"path glob of 256 characters", `{"rules": [{"name": "r", "paths": ["` + strings.Repeat("g", 256) + `"], "require": {}}]}`, at("r", 0, "paths[0]")},
		{"paths a string", `{"rules": [{"name": "r", "paths": "docs/**", "require": {}}]}`, at("r", 0, "paths")},
		{"path glob not a string", `{"rules": [{"name": "r", "paths": [3], "require": {}}]}`, at("r", 0, "paths[0]")},
		{"absolute exclude", inRule(`"exclude": ["/x"], "require": {}`), at("r", 0, "exclude[0]")},
		{"exclude a string", inRule(`"exclude": "x", "require": {}`), at("r", 0, "exclude")},
		{"empty targets", inRule(`"targets": [], "require": {}`), at("r", 0, "targets")},
		{"target with a space", inRule(`"targets": ["a b"], "require": {}`), at("r", 0, "targets[0]")},
		{"target with ..", inRule(`"targets": ["a..b"], "require": {}`), at("r", 0, "targets[0]")},
		{"target ending .lock", inRule(`"targets": ["x.lock"], "require": {}`), at("r", 0, "targets[0]")},
		{"target starting with -", inRule(`"targets": ["-x"], "require": {}`), at("r", 0, "targets[0]")},
		{"target with ?", inRule(`"targets": ["a?"], "require": {}`), at("r", 0, "targets[0]")},
		{"rule without require", inRule(`"exclude": []`), at("r", 0, "require")},
		{"require a list", inRule(`"require": []`), at("r", 0, "require")},
		{"rule not an object", `{"rules": ["docs"]}`, violation{nil, new(0), "rules[0]"}},
	}
	// Each requirement violation once inside a rule and once in default.
	requirements := []struct{ name, members, field string }{
		{"approvals -1", `"approvals": -1`, "approvals"},
		{"approvals 11", `"approvals": 11`, "approvals"},
		{"approvals 1.5", `"approvals": 1.5`, "approvals"},
		{"approvals a string", `"approvals": "1"`, "approvals"},
		{"empty approvers", `"approvers": []`, "approvers"},
		{"approver of a team", `"approvers": ["team:x"]`, "approvers[0]"},
		{"approver user: without a name", `"approvers": ["user:"]`, "approvers[0]"},
		{"approver not a string", `"approvers": [3]`, "approvers[0]"},
		{"approvers a string", `"approvers": "owner"`, "approvers"},
		{"check upper case", `"checks": ["Test"]`, "checks[0]"},
		{"empty check", `"checks": [""]`, "checks[0]"},
		{"check of 64 characters", `"checks": ["` + strings.Repeat("c", 64) + `"]`, "checks[0]"},
		{"checks a string", `"checks": "test"`, "checks"},
		{"fresh_checks a string", `"fresh_checks": "yes"`, "fresh_checks"},
		{"block a number", `"block": 1`, "block"},
	}
	for _, r := range requirements {
		rows = append(rows,
			row{"rule: " + r.name, inRule(`"require": {` + r.members + `}`), at("r", 0, "require."+r.field)},
			row{"default: " + r.name, `{"default": {` + r.members + `}}`, inDefault(r.field)})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			nf, err := validate(t, tc.raw)
			wantViolation(t, nf, err, tc.want)
		})
	}
}

func TestValidate_RefusesAnApproverWhoIsNoUser(t *testing.T) {
	billing := `{"rules": [{"name": "billing", "paths": ["billing/**"], "require": {"approvers": ["owner", "user:nobody"]}}]}`
	nf, err := validate(t, billing)
	wantViolation(t, nf, err, at("billing", 0, "require.approvers[1]"))

	nf, err = validate(t, `{"default": {"approvers": ["user:nobody"]}}`)
	wantViolation(t, nf, err, inDefault("approvers[0]"))

	for _, name := range []string{"dana", "blocked-bob"} {
		if _, err := validate(t, `{"default": {"approvers": ["user:`+name+`"]}}`); err != nil {
			t.Errorf("approver user:%s: %v", name, err)
		}
	}

	failure := errors.New("disk I/O error")
	nf, err = policy.Validate(context.Background(), []byte(billing), knownUsers{err: failure})
	var invalid *policy.InvalidPolicyError
	if nf != nil || !errors.Is(err, failure) || errors.As(err, &invalid) {
		t.Fatalf("with a failing lookup: %v, %v; want the lookup's failure", nf, err)
	}
}

// rules is a document of n rules named r0 to r(n-1).
func rules(n int) string {
	list := make([]string, n)
	for i := range list {
		list[i] = fmt.Sprintf(`{"name": "r%d", "paths": ["a/**"], "require": {}}`, i)
	}
	return `{"rules": [` + strings.Join(list, ",") + `]}`
}

// padded is raw followed by white space up to size bytes.
func padded(raw string, size int) string { return raw + strings.Repeat(" ", size-len(raw)) }

func TestValidate_LimitsTheRuleCountAndTheSize(t *testing.T) {
	nf, err := validate(t, rules(200))
	if err != nil || len(nf.Rules) != 200 {
		t.Fatalf("200 rules: %v", err)
	}
	nf, err = validate(t, rules(201))
	wantViolation(t, nf, err, violation{field: "rules"})

	if _, err := validate(t, padded(inRule(`"require": {}`), 65536)); err != nil {
		t.Fatalf("65 536 bytes: %v", err)
	}
	nf, err = validate(t, padded(inRule(`"require": {}`), 65537))
	wantViolation(t, nf, err, violation{field: "document"})
	nf, err = validate(t, padded(rules(201), 65537))
	wantViolation(t, nf, err, violation{field: "document"})
}

func TestValidate_ReportsTheFirstViolation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want violation
	}{
		{"default before the rules", `{"default": {"approvals": 11}, "rules": [{"name": "X", "paths": ["a"], "require": {}}]}`, inDefault("approvals")},
		{"rules in order", `{"rules": [{"name": "a", "paths": [], "require": {}}, {"name": "X", "paths": ["a"], "require": {}}]}`, at("a", 0, "paths")},
		{"name before paths", `{"rules": [{"name": "X", "paths": [], "require": {}}]}`, at("X", 0, "name")},
		{"exclude before require", inRule(`"exclude": ["/a"], "require": {"approvals": 11}`), at("r", 0, "exclude[0]")},
		{"approvers before checks", `{"default": {"checks": ["A"], "approvers": []}}`, inDefault("approvers")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for range 20 {
				nf, err := validate(t, tc.raw)
				wantViolation(t, nf, err, tc.want)
			}
		})
	}
}

func TestInvalidPolicyError_AnswersInvalidPolicyWithItsDetails(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`[]`, `{"rule":null,"index":null,"field":"document"}`},
		{`{"default": {"block": 1}}`, `{"rule":"default","index":null,"field":"block"}`},
		{inRule(`"require": {"approvals": 11}`), `{"rule":"r","index":0,"field":"require.approvals"}`},
	}
	for _, tc := range cases {
		_, err := validate(t, tc.raw)
		var invalid *policy.InvalidPolicyError
		if !errors.As(err, &invalid) {
			t.Fatalf("%s: err = %v", tc.raw, err)
		}
		rec := httptest.NewRecorder()
		server.WriteError(rec, invalid.APIError())
		var body struct {
			Error struct {
				Code      int             `json:"code"`
				ErrorType string          `json:"error_type"`
				Message   string          `json:"message"`
				Details   json.RawMessage `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 400 || body.Error.ErrorType != "invalid_policy" || body.Error.Message == "" || string(body.Error.Details) != tc.want {
			t.Errorf("%s: %d %s", tc.raw, rec.Code, rec.Body)
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
