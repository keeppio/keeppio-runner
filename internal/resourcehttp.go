package internal

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"
)

// resourceStats summarises the inventory for the env-root view (counts
// surfaced as "in control" KPIs at the top of the dashboard).
type resourceStats struct {
	HostCount   int
	TenantCount int
	ServerCount int
	OpsCount    int
}

// handleResource serves every URL under `/r/...`:
//
//	/r/                    → env root view (overview)
//	/r/<host>              → server / ops host detail
//	/r/<host>/<tenant>     → tenant detail (tenant on that host)
//	/r/<tenant>            → standalone tenant detail (no parent server)
//
// All paths render the same `views/resource.html` template; the data
// passed to the template differs per type. The template's `Selected`
// key drives which tree row is highlighted in the sidebar.
func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/r/")
	rest = strings.Trim(rest, "/")

	inv, err := ReadInventoryTree(s.cfg.RepoPath, s.cfg.Env)
	// On a fresh install where the repo hasn't cloned yet the inventory
	// is empty rather than missing — render an empty env root so the
	// operator at least sees the chrome and can hit Settings → Pull repo.
	if err != nil {
		inv = map[string]HostGroup{}
	}

	// Path empty → env-root.
	if rest == "" {
		stats := computeStats(inv)
		s.render(w, "resource.html", map[string]any{
			"User":       currentUser(r),
			"Selected":   "",
			"NavSection": "resource",
			"ViewType":   "env-root",
			"Stats":      stats,
		})
		return
	}

	tab := r.URL.Query().Get("tab")

	parts := strings.SplitN(rest, "/", 2)
	hostName := parts[0]
	host, hostGroup, found := lookupHost(inv, hostName)
	if !found {
		http.NotFound(w, r)
		return
	}

	// One segment: server, ops box, or standalone tenant.
	if len(parts) == 1 {
		switch hostGroup {
		case "clients":
			parent := lookupParentServer(inv, host)
			s.render(w, "resource.html", map[string]any{
				"User":               currentUser(r),
				"Selected":           tenantTreeID(parent, host.Name),
				"NavSection":         "resource",
				"ViewType":           "tenant",
				"Resource":           hostWithGroup(host, hostGroup),
				"ParentServer":       parent,
				"DisabledDomainsSet": disabledDomainsSet(s.cfg.RepoPath, s.cfg.Env, host.Name),
				"ServiceVersions":    ReadServiceVersions(s.cfg.RepoPath, s.cfg.Env, host.Name),
				"Toolbar":            s.cat.BuildToolbar(hostGroup, host.Name),
				"Tab":                normalizeTab(tab, []string{"overview", "domains", "containers", "tasks"}),
				"ScopedTasks":        s.recentTasksForResource(r.Context(), host.Name, hostGroup),
			})
		case "servers":
			tenants := tenantsOnHost(inv, host.Name)
			s.render(w, "resource.html", map[string]any{
				"User":          currentUser(r),
				"Selected":      host.Name,
				"NavSection":    "resource",
				"ViewType":      "host-server",
				"Resource":      hostWithGroup(host, hostGroup),
				"TenantsOnHost": tenants,
				"Toolbar":       s.cat.BuildToolbar(hostGroup, host.Name),
				"Tab":           normalizeTab(tab, []string{"overview", "tenants", "orphans", "tasks"}),
				"ScopedTasks":   s.recentTasksForResource(r.Context(), host.Name, hostGroup),
			})
		default:
			s.render(w, "resource.html", map[string]any{
				"User":        currentUser(r),
				"Selected":    host.Name,
				"NavSection":  "resource",
				"ViewType":    "host-ops",
				"Resource":    hostWithGroup(host, hostGroup),
				"Toolbar":     s.cat.BuildToolbar(hostGroup, host.Name),
				"Tab":         normalizeTab(tab, []string{"overview", "tasks"}),
				"ScopedTasks": s.recentTasksForResource(r.Context(), host.Name, hostGroup),
			})
		}
		return
	}

	// Two segments: explicit server/tenant pairing.
	tenantName := parts[1]
	if hostGroup != "servers" {
		http.NotFound(w, r)
		return
	}
	tenant, tGroup, ok := lookupHost(inv, tenantName)
	if !ok || tGroup != "clients" {
		http.NotFound(w, r)
		return
	}
	// Verify the tenant actually claims this server (defence-in-depth
	// against operators URL-mashing two unrelated names; tree links
	// always match).
	expectedSrv := tenant.OnServerOriginal
	if expectedSrv == "" {
		expectedSrv = tenant.OnServer
	}
	if expectedSrv != "" && expectedSrv != host.Name {
		http.Redirect(w, r, "/r/"+expectedSrv+"/"+tenant.Name, http.StatusFound)
		return
	}
	s.render(w, "resource.html", map[string]any{
		"User":               currentUser(r),
		"Selected":           host.Name + "/" + tenant.Name,
		"NavSection":         "resource",
		"ViewType":           "tenant",
		"Resource":           hostWithGroup(tenant, tGroup),
		"ParentServer":       host,
		"DisabledDomainsSet": disabledDomainsSet(s.cfg.RepoPath, s.cfg.Env, tenant.Name),
		"ServiceVersions":    ReadServiceVersions(s.cfg.RepoPath, s.cfg.Env, tenant.Name),
		"Toolbar":            s.cat.BuildToolbar(tGroup, tenant.Name),
		"Tab":                normalizeTab(tab, []string{"overview", "domains", "containers", "tasks"}),
		"ScopedTasks":        s.recentTasksForResource(r.Context(), tenant.Name, tGroup),
	})
}

// scopedTask is the lightweight row shape consumed by the resource page's
// Tasks tab. We deliberately don't reuse taskRow because the *time.Time
// pointers there are awkward to template against, and the resource page
// only needs a handful of fields.
type scopedTask struct {
	ID        int64
	ActionID  string
	Label     string
	Status    string
	Username  string
	CreatedAt time.Time
	EndedAt   time.Time
}

// recentTasksForSlug returns the 25 most recent tasks relevant to the
// given resource. Two ways a task can match:
//
//  1. args_json contains "<slug>" as a JSON string value -- catches every
//     named arg the runner passes (tenant, target_host, server_name,
//     ops_name, target_server, ...). Wrapped in quotes to anchor matches
//     so "demo" doesn't catch "demo2" or "predemoted".
//
//  2. The resource's group is ops/servers and the task ran an action
//     declared to apply to that group with no field binding (e.g.
//     `runner-self-update`, `provision-ops`). Those actions don't put
//     the host name in args -- they implicitly act on every host in the
//     group, and an env always has one ops host, so listing them on the
//     ops page is what an operator expects.
//
// Errors are intentionally swallowed: an empty Tasks tab is far better
// than crashing the entire resource page when the DB is briefly busy.
func (s *Server) recentTasksForSlug(ctx context.Context, slug string) []scopedTask {
	return s.recentTasksForResource(ctx, slug, "")
}

func (s *Server) recentTasksForResource(ctx context.Context, slug, group string) []scopedTask {
	if slug == "" {
		return nil
	}
	where := []string{"args_json LIKE ?"}
	args := []any{"%\"" + slug + "\"%"}
	if group != "" {
		if implicit := s.cat.ActionsImplicitlyTargeting(group); len(implicit) > 0 {
			placeholders := strings.Repeat("?,", len(implicit))
			placeholders = strings.TrimSuffix(placeholders, ",")
			where = append(where, "action_id IN ("+placeholders+")")
			for _, id := range implicit {
				args = append(args, id)
			}
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, action_id, action_label, status, username, created_at, ended_at
		 FROM tasks
		 WHERE `+strings.Join(where, " OR ")+`
		 ORDER BY id DESC
		 LIMIT 25`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []scopedTask
	for rows.Next() {
		var t scopedTask
		var ended sql.NullTime
		if err := rows.Scan(&t.ID, &t.ActionID, &t.Label, &t.Status, &t.Username, &t.CreatedAt, &ended); err != nil {
			return nil
		}
		if ended.Valid {
			t.EndedAt = ended.Time
		}
		out = append(out, t)
	}
	return out
}

// normalizeTab returns the requested tab if it's allowed, otherwise the
// first allowed tab (treated as default).
func normalizeTab(requested string, allowed []string) string {
	if requested != "" {
		for _, t := range allowed {
			if t == requested {
				return t
			}
		}
	}
	if len(allowed) == 0 {
		return ""
	}
	return allowed[0]
}

// hostView is HostEntry + the resolved inventory group for the
// template (HostEntry itself only carries Group implicitly via its
// containing map key).
type hostView struct {
	HostEntry
	Group string
}

func hostWithGroup(h HostEntry, group string) hostView {
	return hostView{HostEntry: h, Group: group}
}

func lookupHost(inv map[string]HostGroup, name string) (HostEntry, string, bool) {
	for groupName, group := range inv {
		for _, h := range group {
			if h.Name == name {
				return h, groupName, true
			}
		}
	}
	return HostEntry{}, "", false
}

// lookupParentServer returns the server-group host that a tenant claims
// via on_server (preferred) or co-located IP fallback. nil if neither
// resolves into the inventory.
func lookupParentServer(inv map[string]HostGroup, t HostEntry) *HostEntry {
	srv := t.OnServerOriginal
	if srv == "" {
		srv = t.OnServer
	}
	if srv == "" {
		return nil
	}
	for _, s := range inv["servers"] {
		if s.Name == srv {
			return &s
		}
	}
	return nil
}

func tenantsOnHost(inv map[string]HostGroup, server string) []HostEntry {
	var out []HostEntry
	for _, t := range inv["clients"] {
		ref := t.OnServerOriginal
		if ref == "" {
			ref = t.OnServer
		}
		if ref == server {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func tenantTreeID(parent *HostEntry, tenant string) string {
	if parent == nil {
		return tenant
	}
	return parent.Name + "/" + tenant
}

// disabledDomainsSet returns a map keyed by FQDN for fast template
// lookup ({{ index $.DisabledDomainsSet .Fqdn }} → bool).
func disabledDomainsSet(repo, env, slug string) map[string]bool {
	list, err := ReadDisabledDomains(repo, env, slug)
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, d := range list {
		out[d] = true
	}
	return out
}

func computeStats(inv map[string]HostGroup) resourceStats {
	st := resourceStats{}
	for groupName, hosts := range inv {
		switch groupName {
		case "clients":
			st.TenantCount += len(hosts)
		case "servers":
			st.ServerCount += len(hosts)
		case "ops":
			st.OpsCount += len(hosts)
		}
		st.HostCount += len(hosts)
	}
	return st
}

// handleLegacyInventoryRedirect turns the old `/inventory[/<group>/<name>]`
// URLs into their `/r/...` equivalents so existing bookmarks keep
// working. Status 302 (not 301) — keeps the door open for further
// route iteration without poisoning client caches.
func (s *Server) handleLegacyInventoryRedirect(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/inventory")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		http.Redirect(w, r, "/r/", http.StatusFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	// Old path was /inventory/<group>/<name>; new path drops the group
	// (group is derivable from the inventory).
	if len(parts) >= 2 {
		http.Redirect(w, r, "/r/"+parts[1], http.StatusFound)
		return
	}
	http.Redirect(w, r, "/r/", http.StatusFound)
}

// handleHomeRedirect points the bare "/" at the env root view. Kept
// separate from the resource handler so logged-out users still hit
// requireAuth → /login on /, and so the auth middleware doesn't have
// to special-case this path.
func (s *Server) handleHomeRedirect(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/r/", http.StatusFound)
}

