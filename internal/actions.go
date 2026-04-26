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
	ID          string  `yaml:"id"`
	Label       string  `yaml:"label"`
	Description string  `yaml:"description"`
	Group       string  `yaml:"group"` // dashboard category
	Playbook    string  `yaml:"playbook"`
	ExtraArgs   []string `yaml:"extra_args"` // appended raw to ansible-playbook
	Fields      []Field `yaml:"fields"`
	// Severity hints the UI to colour the button (and ask for an extra
	// confirm step). "danger" for things like Decommission tenant.
	Severity string `yaml:"severity"`
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
	path := filepath.Join(repo, "inventories", env, "hosts.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		All struct {
			Children map[string]struct {
				Hosts map[string]any `yaml:"hosts"`
			} `yaml:"children"`
		} `yaml:"all"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	g, ok := doc.All.Children[group]
	if !ok {
		return []string{}, nil
	}
	out := make([]string, 0, len(g.Hosts))
	for k := range g.Hosts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
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
