package sandbox

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// ProfilesRoute declares GET /sandbox-profiles: every profile of the
// configuration, in name order, on one page. It needs the scope
// sandboxes:read of a PAT and access to no workspace, takes no query
// parameter (it ignores any), and answers from the loaded configuration
// alone, so it keeps answering while the runtime is unreachable.
func (s *Service) ProfilesRoute() server.Route {
	return s.Route(server.Route{
		Method:  http.MethodGet,
		Pattern: "/sandbox-profiles",
		Scope:   scopeRead,
		Handler: s.listProfiles,
	})
}

// profileList is the answer of GET /sandbox-profiles: a list of one page,
// whose next_cursor is always null.
type profileList struct {
	Items      []profileItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// profileItem is what a caller learns of a profile. It leaves out the
// policy file's path, the env values and the two expose lists: an agent
// picks a profile by what it offers, and those may name or hold secrets.
type profileItem struct {
	Name                string          `json:"name"`
	Default             bool            `json:"default"`
	Description         string          `json:"description"`
	Image               string          `json:"image"`
	CPU                 string          `json:"cpu"`
	Memory              string          `json:"memory"`
	GPU                 int             `json:"gpu"`
	TTL                 string          `json:"ttl"`
	TTLSeconds          int64           `json:"ttl_seconds"`
	IdleTimeout         string          `json:"idle_timeout"`
	PolicySummary       []string        `json:"policy_summary"`
	Credentials         []string        `json:"credentials"`
	OptionalCredentials []string        `json:"optional_credentials"`
	ExposedSecrets      []exposedSecret `json:"exposed_secrets"`
	HubAccess           string          `json:"hub_access"`
	EnvKeys             []string        `json:"env_keys"`
	Checkpoint          bool            `json:"checkpoint"`
	AllowProposals      bool            `json:"allow_proposals"`
}

// exposedSecret is the name of a secret a profile exposes, and whether a
// sandbox starts without it.
type exposedSecret struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"`
}

func (s *Service) listProfiles(w http.ResponseWriter, _ *http.Request) error {
	profiles := s.settings.Profiles()
	items := make([]profileItem, 0, len(profiles))
	for _, p := range profiles {
		items = append(items, newProfileItem(p))
	}
	server.WriteJSON(w, http.StatusOK, profileList{Items: items})
	return nil
}

func newProfileItem(p Profile) profileItem {
	secrets := make([]exposedSecret, 0, len(p.ExposeSecrets)+len(p.OptionalExposeSecrets))
	for _, n := range p.ExposeSecrets {
		secrets = append(secrets, exposedSecret{Name: n})
	}
	for _, n := range p.OptionalExposeSecrets {
		secrets = append(secrets, exposedSecret{Name: n, Optional: true})
	}
	slices.SortFunc(secrets, func(a, b exposedSecret) int { return cmp.Compare(a.Name, b.Name) })
	hosts := slices.Clone(p.PolicyHosts)
	slices.Sort(hosts)
	return profileItem{
		Name:                p.Name,
		Default:             p.Default,
		Description:         p.Description,
		Image:               p.Image,
		CPU:                 p.CPU,
		Memory:              p.Memory,
		GPU:                 p.GPU,
		TTL:                 formatDuration(p.TTL),
		TTLSeconds:          int64(p.TTL / time.Second),
		IdleTimeout:         formatDuration(p.IdleTimeout),
		PolicySummary:       orEmpty(hosts),
		Credentials:         orEmpty(slices.Clone(p.Credentials)),
		OptionalCredentials: orEmpty(slices.Clone(p.OptionalCredentials)),
		ExposedSecrets:      secrets,
		HubAccess:           p.HubAccess,
		EnvKeys:             orEmpty(slices.Sorted(maps.Keys(p.Env))),
		Checkpoint:          p.Checkpoint,
		AllowProposals:      p.AllowProposals,
	}
}

// orEmpty is l, or an empty list for nil, so that a list member is written
// [] and never null.
func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// durationUnits are the units of the duration grammar (timefmt) above the
// second, largest first.
var durationUnits = []struct {
	suffix string
	length time.Duration
}{
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
}

// formatDuration writes d in the duration grammar the configuration is read
// with, in the largest unit that divides it: 4h, 30m, 1d; zero is 0s. The
// configuration holds whole seconds only; a fraction of a second, which it
// cannot hold, is cut off.
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d <= 0 {
		return "0s"
	}
	for _, u := range durationUnits {
		if d%u.length == 0 {
			return strconv.FormatInt(int64(d/u.length), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}
