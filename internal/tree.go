package internal

import (
	"fmt"
	"sort"
)

// TreeNode is one row in the left sidebar resource tree. It's a plain
// data carrier — the template walks it recursively. URLs are pre-built
// here so the template stays dumb.
type TreeNode struct {
	ID       string      // stable identifier used for selection matching
	Type     string      // env|host-server|host-ops|tenant|section|domain
	Group    string      // inventory group name (clients|servers|ops|...) where applicable
	Label    string
	Sublabel string
	Href     string
	Icon     string      // logical name resolved by templates/_icons.html
	Status   string      // pill class suffix: ok|warn|bad|info — empty = no pill
	Selected bool
	Expanded bool
	Children []*TreeNode
}

// BuildResourceTree walks the inventory and assembles the env → host →
// tenant → domains hierarchy used by the new UI.
//
// Only one env appears (the runner is bound to a single env via cfg.Env),
// and the tree is materialised eagerly: all hosts + per-tenant FQDNs +
// disabled-domain state are pulled in one pass. Containers are NOT
// fetched here (would require SSH per tenant on every render); the
// resource state cache handles that lazily in phase 9.
//
// `selectedID` is the URL path slug after `/r/` — used to mark the
// current node and auto-expand its ancestors. Empty = no selection.
func BuildResourceTree(env, repo, selectedID string) (*TreeNode, error) {
	inv, err := ReadInventoryTree(repo, env)
	if err != nil {
		return nil, err
	}

	root := &TreeNode{
		ID:       "",
		Type:     "env",
		Label:    env,
		Icon:     "globe",
		Expanded: true,
		Href:     "/r/",
	}

	tenantsByServer := map[string][]HostEntry{}
	var standaloneTenants []HostEntry
	var serverHosts []HostEntry
	var opsHosts []HostEntry
	var unknownHosts []HostEntry

	for groupName, hosts := range inv {
		switch groupName {
		case "clients":
			for _, h := range hosts {
				srv := h.OnServerOriginal
				if srv == "" {
					srv = h.OnServer
				}
				if srv != "" && srv != h.Name {
					tenantsByServer[srv] = append(tenantsByServer[srv], h)
				} else {
					standaloneTenants = append(standaloneTenants, h)
				}
			}
		case "servers":
			serverHosts = append(serverHosts, hosts...)
		case "ops":
			opsHosts = append(opsHosts, hosts...)
		default:
			// Future / custom groups land in their own section — not
			// silently dropped, since operators may have added them
			// deliberately.
			unknownHosts = append(unknownHosts, hosts...)
		}
	}

	sort.Slice(serverHosts, func(i, j int) bool { return serverHosts[i].Name < serverHosts[j].Name })
	sort.Slice(opsHosts, func(i, j int) bool { return opsHosts[i].Name < opsHosts[j].Name })
	sort.Slice(standaloneTenants, func(i, j int) bool { return standaloneTenants[i].Name < standaloneTenants[j].Name })
	sort.Slice(unknownHosts, func(i, j int) bool { return unknownHosts[i].Name < unknownHosts[j].Name })

	// --- Servers (with their tenants nested) ---
	serverNameSet := make(map[string]bool, len(serverHosts))
	for _, srv := range serverHosts {
		serverNameSet[srv.Name] = true
	}
	for _, srv := range serverHosts {
		srvNode := &TreeNode{
			ID:       srv.Name,
			Type:     "host-server",
			Group:    "servers",
			Label:    srv.Name,
			Sublabel: srv.Host,
			Href:     "/r/" + srv.Name,
			Icon:     "server",
		}
		tenants := tenantsByServer[srv.Name]
		sort.Slice(tenants, func(i, j int) bool { return tenants[i].Name < tenants[j].Name })
		for _, t := range tenants {
			srvNode.Children = append(srvNode.Children, buildTenantNode(repo, env, srv.Name, t))
		}
		if n := len(srvNode.Children); n > 0 {
			srvNode.Sublabel = fmt.Sprintf("%s · %d tenant%s", srv.Host, n, plural(n))
		}
		root.Children = append(root.Children, srvNode)
	}

	// Synthetic server nodes for "consumed" servers: a tenant onboard
	// moves the server entry from `servers` -> `clients` (renamed to
	// the tenant slug), so the original server name is no longer in
	// inventory. Operators still think of the tenant as living on
	// "<original-server-name>", so render a synthetic parent for each
	// such server name with the tenants nested under it. The parent
	// is not clickable (no Href) -- it's purely a grouping affordance.
	// IP is inferred from the first tenant's ansible_host, which is
	// the IP of the consumed VPS.
	consumedServerNames := []string{}
	for srvName := range tenantsByServer {
		if !serverNameSet[srvName] {
			consumedServerNames = append(consumedServerNames, srvName)
		}
	}
	sort.Strings(consumedServerNames)
	for _, srvName := range consumedServerNames {
		tenants := tenantsByServer[srvName]
		sort.Slice(tenants, func(i, j int) bool { return tenants[i].Name < tenants[j].Name })
		ip := ""
		if len(tenants) > 0 {
			ip = tenants[0].Host
		}
		sub := ip
		if n := len(tenants); n > 0 {
			if ip != "" {
				sub = fmt.Sprintf("%s · %d tenant%s", ip, n, plural(n))
			} else {
				sub = fmt.Sprintf("%d tenant%s", n, plural(n))
			}
		}
		srvNode := &TreeNode{
			ID:       srvName,
			Type:     "host-server",
			Group:    "servers",
			Label:    srvName,
			Sublabel: sub,
			Icon:     "server",
			// no Href -- the server entry no longer exists in inventory,
			// so /r/<srvName> would 404. The tenant rows below remain
			// independently clickable.
			Expanded: true,
		}
		for _, t := range tenants {
			// Pass empty serverSlug so the tenant URL stays /r/<slug>;
			// /r/<consumed-server>/<tenant> would 404 because the server
			// host has no inventory entry.
			srvNode.Children = append(srvNode.Children, buildTenantNode(repo, env, "", t))
		}
		root.Children = append(root.Children, srvNode)
	}
	sort.Slice(standaloneTenants, func(i, j int) bool { return standaloneTenants[i].Name < standaloneTenants[j].Name })

	// Ops, standalone tenants, and unknown hosts all sit at the same
	// depth as server hosts (no wrapping section node). Type icons in
	// the tree row already differentiate them visually — the extra
	// folder layer just hid hosts behind an unnecessary click.

	// --- Ops infrastructure ---
	for _, o := range opsHosts {
		root.Children = append(root.Children, &TreeNode{
			ID:       o.Name,
			Type:     "host-ops",
			Group:    "ops",
			Label:    o.Name,
			Sublabel: o.Host,
			Href:     "/r/" + o.Name,
			Icon:     "tool",
		})
	}

	// --- Standalone tenants (whose registered server isn't in this env) ---
	for _, t := range standaloneTenants {
		root.Children = append(root.Children, buildTenantNode(repo, env, "", t))
	}

	// --- Unknown / future groups ---
	for _, h := range unknownHosts {
		root.Children = append(root.Children, &TreeNode{
			ID:       h.Name,
			Type:     "host-ops",
			Label:    h.Name,
			Sublabel: h.Host,
			Href:     "/r/" + h.Name,
			Icon:     "box",
		})
	}

	if selectedID != "" {
		markSelected(root, selectedID)
	}

	return root, nil
}

// buildTenantNode constructs a tenant subtree, with a "Domains" child
// section listing each FQDN and its on/off status. `serverSlug` may be
// empty for standalone tenants — in that case the resource URL is just
// `/r/<tenant>`, which is unambiguous because tenant slugs never collide
// with server/ops slugs in a single inventory.
func buildTenantNode(repo, env, serverSlug string, t HostEntry) *TreeNode {
	tenantPath := t.Name
	if serverSlug != "" {
		tenantPath = serverSlug + "/" + t.Name
	}
	node := &TreeNode{
		ID:       tenantPath,
		Type:     "tenant",
		Group:    "clients",
		Label:    t.Name,
		Sublabel: t.PrimaryFqdn,
		Href:     "/r/" + tenantPath,
		Icon:     "tenant",
	}

	disabled, _ := ReadDisabledDomains(repo, env, t.Name)
	disabledSet := make(map[string]bool, len(disabled))
	for _, d := range disabled {
		disabledSet[d] = true
	}

	if len(t.AllFqdns) > 0 {
		domains := &TreeNode{
			ID:       tenantPath + "#domains",
			Type:     "section",
			Label:    "Domains",
			Icon:     "globe",
			Sublabel: fmt.Sprintf("%d", len(t.AllFqdns)),
			// Click takes you to the tenant page's domains tab, where
			// the full FQDN list + on/off toggles render. Without a
			// Href the tree row was a dead label.
			Href: "/r/" + tenantPath + "?tab=domains",
		}
		for _, d := range t.AllFqdns {
			status := "ok"
			if disabledSet[d.Fqdn] {
				status = "bad"
			}
			domains.Children = append(domains.Children, &TreeNode{
				ID:       tenantPath + "#dom:" + d.Label,
				Type:     "domain",
				Label:    d.Fqdn,
				Sublabel: d.Label,
				Icon:     "dot",
				Status:   status,
				// Same destination as the parent -- per-domain detail
				// pages don't exist; landing on the domains tab is
				// what an operator wants either way.
				Href: "/r/" + tenantPath + "?tab=domains",
			})
		}
		node.Children = append(node.Children, domains)
	}

	// "Containers" is rendered as a placeholder leaf that links to the
	// tenant view's Containers tab. Live container state is fetched on
	// demand by the view, not eagerly here (see phase 9 / state cache).
	node.Children = append(node.Children, &TreeNode{
		ID:    tenantPath + "#containers",
		Type:  "section",
		Label: "Containers",
		Icon:  "box",
		Href:  "/r/" + tenantPath + "?tab=containers",
	})

	return node
}

// markSelected walks the tree, sets Selected on the matched node, and
// flips Expanded=true on every ancestor so the path renders open. If
// the id isn't found, leaves the tree untouched.
func markSelected(n *TreeNode, id string) bool {
	if n.ID == id {
		n.Selected = true
		n.Expanded = true
		return true
	}
	for _, c := range n.Children {
		if markSelected(c, id) {
			n.Expanded = true
			return true
		}
	}
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
