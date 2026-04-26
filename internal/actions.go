package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Action is one button on the dashboard. Maps 1:1 to a playbook in
// keeppio-infrastructure. Loaded from `actions.yml` at the repo root.
type Action struct {
	ID          string   `yaml:"id"`
	Label       string   `yaml:"label"`
	Description string   `yaml:"description"`
	Group       string   `yaml:"group"` // dashboard category
	Playbook    string   `yaml:"playbook"`
	ExtraArgs   []string `yaml:"extra_args"` // appended raw to ansible-playbook
	Fields      []Field  `yaml:"fields"`
	// Severity hints the UI to colour the button (and ask for an extra
	// confirm step). "danger" for things like Decommission tenant.
	Severity string `yaml:"severity"`
	// AppliesTo declares which inventory resource(s) the action acts
	// on, plus the form field that should be pre-populated when the
	// user reaches the action from a resource detail page. An action
	// can apply to multiple groups (e.g. tenant-move acts on both a
	// tenant and a target server). Field can be empty for actions
	// that don't need a per-host param (e.g. runner-self-update).
	AppliesTo []Applies `yaml:"applies_to"`
	// CreatesIn means "this action creates a new resource in the
	// named inventory group" — the inventory page renders a "+" link
	// to this action's form next to the matching group header. At
	// most one creator per group; ties resolve to the alphabetically
	// first action ID.
	CreatesIn string `yaml:"creates_in"`
}

type Applies struct {
	Group string `yaml:"group"` // inventory group name (clients|servers|ops)
	Field string `yaml:"field"` // form field to pre-fill; empty if N/A
}

type Field struct {
	Name        string  `yaml:"name"`
	Label       string  `yaml:"label"`
	Description string  `yaml:"description"`
	Type        string  `yaml:"type"`        // string|int|secret|enum|bool
	Required    bool    `yaml:"required"`
	Default     string  `yaml:"default"`
	Pattern     string  `yaml:"pattern"`     // regex, validated server-side
	MustMatch   string  `yaml:"must_match"`  // peer-field name; values must equal
	Source      *Source `yaml:"source"`      // for type=enum
}

type Source struct {
	Kind    string   `yaml:"kind"`    // static|inventory|ghcr-tags
	Group   string   `yaml:"group"`   // for inventory: clients|servers
	Package string   `yaml:"package"` // for ghcr-tags
	Values  []string `yaml:"values"`  // for static
}

// Catalog is loaded from disk + reloaded on a `git pull`. Read access
// is concurrent; reload locks briefly.
type Catalog struct {
	mu      sync.RWMutex
	actions []Action
}

func (c *Catalog) Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc struct {
		Actions []Action `yaml:"actions"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for _, a := range doc.Actions {
		if a.ID == "" || a.Label == "" || a.Playbook == "" {
			return fmt.Errorf("action missing id/label/playbook: %+v", a)
		}
	}
	c.mu.Lock()
	c.actions = doc.Actions
	c.mu.Unlock()
	return nil
}

func (c *Catalog) All() []Action {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Action, len(c.actions))
	copy(out, c.actions)
	return out
}

func (c *Catalog) ByID(id string) (Action, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.actions {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// Grouped returns actions partitioned by Group, with stable label
// ordering inside each group. Used by the dashboard.
func (c *Catalog) Grouped() []GroupedActions {
	all := c.All()
	bag := map[string][]Action{}
	for _, a := range all {
		g := a.Group
		if g == "" {
			g = "General"
		}
		bag[g] = append(bag[g], a)
	}
	groups := make([]string, 0, len(bag))
	for g := range bag {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	out := make([]GroupedActions, 0, len(groups))
	for _, g := range groups {
		as := bag[g]
		sort.Slice(as, func(i, j int) bool { return as[i].Label < as[j].Label })
		out = append(out, GroupedActions{Group: g, Actions: as})
	}
	return out
}

type GroupedActions struct {
	Group   string
	Actions []Action
}

// ──────────────────────────────────────────────────────────────────────
// Source resolvers
// ──────────────────────────────────────────────────────────────────────

// ResolveOptions returns the dropdown values for a field whose source
// is dynamic. Pure-static enums short-circuit and return Source.Values.
// Errors here become a friendly inline message in the form.
func ResolveOptions(ctx context.Context, cfg *Config, f Field) ([]string, error) {
	if f.Source == nil {
		return nil, nil
	}
	switch f.Source.Kind {
	case "static":
		return f.Source.Values, nil
	case "inventory":
		return readInventoryGroup(cfg.RepoPath, cfg.Env, f.Source.Group)
	case "ghcr-tags":
		return ghcrTags(ctx, cfg.GitHubPAT, f.Source.Package)
	default:
		return nil, fmt.Errorf("unknown source kind %q", f.Source.Kind)
	}
}

func readInventoryGroup(repo, env, group string) ([]string, error) {
	tree, err := ReadInventoryTree(repo, env)
	if err != nil {
		return nil, err
	}
	if hosts, ok := tree[group]; ok {
		return hosts.Names(), nil
	}
	return []string{}, nil
}

// HostEntry is one entry under a `hosts:` key in hosts.yml plus a few
// fields lifted out of host_vars/<name>/vars.yml. Loaded eagerly so
// the inventory pages can show domains and other context without each
// page handler re-parsing the same yaml files.
type HostEntry struct {
	Name string
	Host string
	Port int
	User string
	// PrimaryFqdn is the most useful "where does this thing live"
	// FQDN. For tenants we use webapp_fqdn; for ops boxes the runner
	// FQDN. Empty for hosts that don't have one (registered-but-
	// unclaimed servers).
	PrimaryFqdn string
	// AllFqdns is the complete list (ordered: webapp, api, bridge,
	// paynl, reverb). Used by the drill-down page.
	AllFqdns []FqdnEntry
	// OnServer is the name of another inventory host (preferring
	// `servers` group, then `ops`, then `clients`) that lives on the
	// same IP. Lets the inventory cards show "user@<server-name>"
	// instead of the bare IP. Empty when no co-resident is found.
	OnServer string
	// Raw is whatever the YAML had under the hosts.yml entry — kept
	// for future use.
	Raw map[string]any
}

type FqdnEntry struct {
	Label string // "webapp", "api", …
	Fqdn  string
}

// HostGroup is one of clients/servers/ops/... A wrapper around a list
// of HostEntry with stable ordering.
type HostGroup []HostEntry

func (g HostGroup) Names() []string {
	out := make([]string, 0, len(g))
	for _, h := range g {
		out = append(out, h.Name)
	}
	return out
}

// ReadDeployPubkey reads the env's deploy SSH public key from
// inventories/<env>/group_vars/all/vars.yml. Returns "" if missing.
// Plain (non-vault) YAML — no decryption needed.
func ReadDeployPubkey(repo, env string) string {
	path := filepath.Join(repo, "inventories", env, "group_vars", "all", "vars.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return ""
	}
	if v, ok := doc["deploy_ssh_public_key"].(string); ok {
		return v
	}
	return ""
}

// ReadInventoryTree returns every group in the env's hosts.yml. Used
// by the inventory page (browse) and by the per-field source resolver.
func ReadInventoryTree(repo, env string) (map[string]HostGroup, error) {
	path := filepath.Join(repo, "inventories", env, "hosts.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		All struct {
			Children map[string]struct {
				Hosts map[string]map[string]any `yaml:"hosts"`
			} `yaml:"children"`
		} `yaml:"all"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]HostGroup{}
	for groupName, g := range doc.All.Children {
		hosts := make(HostGroup, 0, len(g.Hosts))
		for name, raw := range g.Hosts {
			h := HostEntry{Name: name, Raw: raw, Port: 22, User: "root"}
			if v, ok := raw["ansible_host"].(string); ok {
				h.Host = v
			}
			if v, ok := raw["ansible_user"].(string); ok {
				h.User = v
			}
			if v, ok := raw["ansible_port"].(int); ok {
				h.Port = v
			}
			h.PrimaryFqdn, h.AllFqdns = readHostFqdns(repo, env, name)
			hosts = append(hosts, h)
		}
		sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })
		out[groupName] = hosts
	}
	// Second pass: populate OnServer by finding a co-resident host.
	// Preference order is intentional — a tenant on a registered
	// server's IP shows that server's name; an ops box on a unique IP
	// shows nothing.
	preference := []string{"servers", "ops", "clients"}
	for groupName, g := range out {
		for i, h := range g {
			for _, pref := range preference {
				if found := pickColocated(out, pref, h, groupName, h.Name); found != "" {
					out[groupName][i].OnServer = found
					break
				}
			}
		}
	}
	return out, nil
}

func pickColocated(tree map[string]HostGroup, group string, h HostEntry, selfGroup, selfName string) string {
	for _, c := range tree[group] {
		if c.Host == h.Host && !(group == selfGroup && c.Name == selfName) {
			return c.Name
		}
	}
	return ""
}

// readHostFqdns pulls FQDN-like fields out of host_vars/<name>/vars.yml.
// We probe a fixed list of well-known field names so each row in the
// inventory page can show the live domain. Missing file → empty
// result. Order matters: the first non-empty becomes the primary FQDN
// shown on the card.
func readHostFqdns(repo, env, name string) (string, []FqdnEntry) {
	path := filepath.Join(repo, "inventories", env, "host_vars", name, "vars.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", nil
	}
	probes := []struct {
		Label string
		Key   string
	}{
		{"webapp", "webapp_fqdn"},
		{"api", "api_fqdn"},
		{"bridge", "bridge_fqdn"},
		{"paynl", "paynl_fqdn"},
		{"reverb", "reverb_fqdn"},
	}
	var primary string
	out := make([]FqdnEntry, 0, len(probes))
	for _, p := range probes {
		v, ok := doc[p.Key].(string)
		if !ok || v == "" {
			continue
		}
		if primary == "" {
			primary = v
		}
		out = append(out, FqdnEntry{Label: p.Label, Fqdn: v})
	}

	// Ops boxes use a nested `runner_instances:` list of
	// {name, fqdn, port, ...}. Walk it so each runner gets its own
	// row. Same shape would naturally extend to other future ops
	// services that follow the *_instances pattern.
	if instances, ok := doc["runner_instances"].([]any); ok {
		for _, raw := range instances {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fqdn, _ := m["fqdn"].(string)
			if fqdn == "" {
				continue
			}
			label, _ := m["name"].(string)
			if label == "" {
				label = "runner"
			}
			if primary == "" {
				primary = fqdn
			}
			out = append(out, FqdnEntry{Label: "runner-" + label, Fqdn: fqdn})
		}
	}
	return primary, out
}

// ghcrTags queries the GitHub container registry for an org-owned
// package's versions and returns canonical semver tags + "main".
// Cached for 60s to keep page renders snappy.
type ghcrCacheEntry struct {
	tags   []string
	at     time.Time
}

var (
	ghcrCacheMu sync.Mutex
	ghcrCache   = map[string]ghcrCacheEntry{}
	semverRe    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

func ghcrTags(ctx context.Context, pat, pkg string) ([]string, error) {
	if pat == "" {
		return []string{"main"}, nil
	}
	ghcrCacheMu.Lock()
	if e, ok := ghcrCache[pkg]; ok && time.Since(e.at) < time.Minute {
		ghcrCacheMu.Unlock()
		return e.tags, nil
	}
	ghcrCacheMu.Unlock()

	url := "https://api.github.com/orgs/keeppio/packages/container/" + pkg + "/versions?per_page=100"
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ghcr %s: %s: %s", pkg, resp.Status, strings.TrimSpace(string(body)))
	}
	var versions []struct {
		Metadata struct {
			Container struct {
				Tags []string `json:"tags"`
			} `json:"container"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{"main": {}}
	for _, v := range versions {
		for _, t := range v.Metadata.Container.Tags {
			if semverRe.MatchString(t) {
				seen[t] = struct{}{}
			}
		}
	}
	tags := make([]string, 0, len(seen))
	for t := range seen {
		tags = append(tags, t)
	}
	// "main" first, then semver descending.
	sort.Slice(tags, func(i, j int) bool {
		if tags[i] == "main" {
			return true
		}
		if tags[j] == "main" {
			return false
		}
		return tags[i] > tags[j]
	})
	ghcrCacheMu.Lock()
	ghcrCache[pkg] = ghcrCacheEntry{tags: tags, at: time.Now()}
	ghcrCacheMu.Unlock()
	return tags, nil
}

// ValidateSubmission checks every field's value against the action
// spec. Returns the first error encountered (form pages show one
// error at a time — keeps the UI calm). On success it returns the
// canonical args map ready to be turned into ansible -e flags.
func ValidateSubmission(a Action, form map[string][]string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range a.Fields {
		raw := strings.TrimSpace(firstOrEmpty(form[f.Name]))
		if raw == "" {
			if f.Required {
				return nil, fmt.Errorf("%s is required", f.Label)
			}
			if f.Default != "" {
				raw = f.Default
			} else {
				continue
			}
		}
		if f.Pattern != "" {
			re, err := regexp.Compile(f.Pattern)
			if err != nil {
				return nil, fmt.Errorf("%s pattern is invalid: %v", f.Label, err)
			}
			if !re.MatchString(raw) {
				return nil, fmt.Errorf("%s doesn't match %s", f.Label, f.Pattern)
			}
		}
		if f.MustMatch != "" {
			peer := strings.TrimSpace(firstOrEmpty(form[f.MustMatch]))
			if raw != peer {
				return nil, fmt.Errorf("%s must equal %s", f.Label, f.MustMatch)
			}
		}
		out[f.Name] = raw
	}
	return out, nil
}

func firstOrEmpty(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
