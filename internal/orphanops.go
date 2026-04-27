package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ──────────────────────────────────────────────────────────────────────
// Orphan containers — sister feature to the per-tenant container
// management in tenantops.go, but operating on a host-level scope.
//
// Operators visit /inventory/servers/<name> or /inventory/ops/<name>
// and see every `keeppio-*` container on the host that is NOT linked
// to any current tenant or expected ops service. Each row gets a
// `docker rm -f` button.
//
// The "expected" set is derived from:
//   1. roles/apps/defaults/main.yml `app_services` (excluding
//      profiles=migrate one-shots) → per-tenant suffixes,
//      plus `postgres` and `nginx` from the postgres/nginx roles.
//   2. ops-only services: `keeppio-<ops>-runner-{instance...}`,
//      `keeppio-<ops>-nginx`, `keeppio-kuma-seed`, and
//      `keeppio-kuma-<slug>` per tenant (the per-tenant Uptime Kuma).
//
// We compute the expected set across the WHOLE inventory (every tenant
// + every ops box) and use it as a deny-list for delete: a container
// listed as "expected" anywhere can never be removed via this UI even
// if a stale copy is sitting on the wrong host. Operators have the
// per-tenant container UI for legitimate stop/start of those.
// ──────────────────────────────────────────────────────────────────────

// hostConn is the bare SSH-target tuple for a server / ops host. Mirrors
// tenantConn but with no slug (the host name is its own scope).
type hostConn struct {
	Name string
	Host string
	User string
	Port int
}

// resolveHost looks up <name> in the inventory and returns its SSH
// target. Refuses anything outside `servers` or `ops` — the orphan-
// container UI only makes sense for host-scoped boxes; for tenants the
// per-tenant container UI already covers it.
func resolveHost(cfg *Config, name string) (hostConn, string, error) {
	tree, err := ReadInventoryTree(cfg.RepoPath, cfg.Env)
	if err != nil {
		return hostConn{}, "", err
	}
	for _, group := range []string{"servers", "ops"} {
		for _, h := range tree[group] {
			if h.Name == name {
				return hostConn{
					Name: h.Name,
					Host: h.Host,
					User: h.User,
					Port: h.Port,
				}, group, nil
			}
		}
	}
	return hostConn{}, "", fmt.Errorf("host %q not found in servers or ops group", name)
}

// hostSSHExec is sshExec but for a hostConn. Kept as a thin alias so
// the call sites here read clearly without leaking tenant terminology.
func hostSSHExec(ctx context.Context, h hostConn, command string) ([]byte, error) {
	return sshExec(ctx, tenantConn{Host: h.Host, User: h.User, Port: h.Port}, command)
}

// ──────────────────────────────────────────────────────────────────────
// Expected-set computation
// ──────────────────────────────────────────────────────────────────────

// appServicesEntry is the slice element in roles/apps/defaults/main.yml
// under `app_services`. Only `name` + `profiles` matter here; we ignore
// everything else (image / env / depends_on / …) so changes to those
// fields don't ripple into the runner.
type appServicesEntry struct {
	Name     string   `yaml:"name"`
	Profiles []string `yaml:"profiles"`
}

// readAppServiceSuffixes parses roles/apps/defaults/main.yml and
// returns the list of service `name`s that map to a real container per
// tenant — i.e. excludes profiled one-shots like portal-migrate /
// paynl-migrate which only run on demand and never have a long-lived
// container.
//
// Returns the literal set on disk plus `postgres` and `nginx`, which
// are separate roles (postgres/, nginx/) that also produce a
// keeppio-<slug>-{postgres,nginx} container per tenant.
func readAppServiceSuffixes(repo string) ([]string, error) {
	path := filepath.Join(repo, "roles", "apps", "defaults", "main.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		AppServices []appServicesEntry `yaml:"app_services"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	suffixes := make([]string, 0, len(doc.AppServices)+2)
	for _, s := range doc.AppServices {
		if s.Name == "" {
			continue
		}
		// Skip migrate (and any future profile-only) one-shots — they
		// never have a long-lived container, so the absence of one isn't
		// "missing", and the presence of one is correctly an orphan
		// (left over from a `compose run` that didn't get --rm'd).
		if hasMigrateProfile(s.Profiles) {
			continue
		}
		suffixes = append(suffixes, s.Name)
	}
	// postgres + nginx are produced by their own roles (postgres/,
	// nginx/) and named keeppio-<slug>-{postgres,nginx}. Add them by
	// hand since they don't appear in app_services.
	suffixes = append(suffixes, "postgres", "nginx")
	sort.Strings(suffixes)
	return suffixes, nil
}

func hasMigrateProfile(profiles []string) bool {
	for _, p := range profiles {
		if p == "migrate" {
			return true
		}
	}
	return false
}

// readRunnerInstanceNames pulls the `runner_instances[].name` list out
// of an ops box's host_vars/<ops>/vars.yml. Used to compute the
// expected `keeppio-<ops>-runner-<instance>` set. Best-effort: missing
// or unparseable file → empty list, the caller falls back to the
// hard-coded {sandbox,staging,production} list per the task spec.
func readRunnerInstanceNames(repo, env, opsName string) []string {
	path := filepath.Join(repo, "inventories", env, "host_vars", opsName, "vars.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		RunnerInstances []struct {
			Name string `yaml:"name"`
		} `yaml:"runner_instances"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil
	}
	out := make([]string, 0, len(doc.RunnerInstances))
	for _, r := range doc.RunnerInstances {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	return out
}

// computeExpectedContainers returns the global set of keeppio-* names
// the inventory says SHOULD exist somewhere. Anything starting
// `keeppio-` not in this set is an orphan. Since we never know which
// containers might end up co-located on a given host (tenant-move
// leaves stragglers, operators rebuild things), we treat the whole
// inventory's expected set as a deny-list across all hosts: never let
// the UI delete a name that is legitimately expected anywhere.
func computeExpectedContainers(cfg *Config) (map[string]bool, error) {
	tree, err := ReadInventoryTree(cfg.RepoPath, cfg.Env)
	if err != nil {
		return nil, err
	}
	suffixes, err := readAppServiceSuffixes(cfg.RepoPath)
	if err != nil {
		return nil, err
	}
	expected := map[string]bool{}

	// Per-tenant containers (apps + postgres + nginx + Kuma).
	tenantSlugs := make([]string, 0, len(tree["clients"]))
	for _, h := range tree["clients"] {
		tenantSlugs = append(tenantSlugs, h.Name)
	}
	for _, slug := range tenantSlugs {
		for _, sfx := range suffixes {
			expected["keeppio-"+slug+"-"+sfx] = true
		}
		// Per-tenant Uptime Kuma container lives on the ops box; named
		// keeppio-kuma-<slug> per roles/uptime-kuma/templates/docker-compose.yml.j2.
		expected["keeppio-kuma-"+slug] = true
	}

	// Per ops-box: nginx + every runner instance + the seed image's
	// throwaway name (it's a `docker run --rm` so usually absent, but
	// don't flag it if we ever catch it mid-run).
	for _, h := range tree["ops"] {
		expected["keeppio-"+h.Name+"-nginx"] = true
		instances := readRunnerInstanceNames(cfg.RepoPath, cfg.Env, h.Name)
		// Fallback per task spec: if the file's missing, assume the two
		// canonical instance names so we don't accidentally orphan-flag
		// a live runner.
		if len(instances) == 0 {
			instances = []string{"sandbox", "staging", "production"}
		}
		for _, inst := range instances {
			expected["keeppio-"+h.Name+"-runner-"+inst] = true
		}
	}
	// Seed container is `docker run --rm keeppio-kuma-seed:latest` —
	// the image, not a long-lived container. But the role builds the
	// image with `-t keeppio-kuma-seed:latest` and `docker ps -a` won't
	// show images. Still, list it as expected just in case an operator
	// runs it without --rm; matches the task's spec.
	expected["keeppio-kuma-seed"] = true

	return expected, nil
}

// ──────────────────────────────────────────────────────────────────────
// SSH list + delete
// ──────────────────────────────────────────────────────────────────────

// ListOrphanContainers SSHes to <hostName>, runs docker ps -a, filters
// to keeppio-* names not in the expected set, and returns them sorted.
func ListOrphanContainers(ctx context.Context, cfg *Config, hostName string) ([]ContainerInfo, error) {
	hc, _, err := resolveHost(cfg, hostName)
	if err != nil {
		return nil, err
	}
	expected, err := computeExpectedContainers(cfg)
	if err != nil {
		return nil, err
	}
	// `docker ps -a --filter name=^/keeppio-` — the leading `^/` anchors
	// docker's regex to the start of the container name (docker prefixes
	// matches with `/`).
	cmd := "docker ps -a --filter name=^/keeppio- --format '{{json .}}'"
	out, err := hostSSHExec(ctx, hc, cmd)
	if err != nil {
		return nil, fmt.Errorf("ssh docker ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var orphans []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var e dockerPSEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if !strings.HasPrefix(e.Names, "keeppio-") {
			continue
		}
		if expected[e.Names] {
			continue
		}
		orphans = append(orphans, ContainerInfo{
			Name:    e.Names,
			Image:   e.Image,
			State:   e.State,
			Status:  e.Status,
			Running: e.State == "running",
			Created: e.CreatedAt,
		})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].Name < orphans[j].Name })
	return orphans, nil
}

// DeleteOrphanContainer runs `docker rm -f <name>` on the host. Refuses
// if the name is not a keeppio-* (defense in depth), or if the name
// IS in the expected set (don't let a stale path through here delete
// a real tenant/ops container — the per-tenant UI is the right tool
// for stopping those).
func DeleteOrphanContainer(ctx context.Context, cfg *Config, hostName, container string) ([]byte, error) {
	hc, _, err := resolveHost(cfg, hostName)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(container, "keeppio-") {
		return nil, fmt.Errorf("container name %q is not under the keeppio- prefix", container)
	}
	if strings.ContainsAny(container, "`;&|$<>\"'\\ \t\n") {
		return nil, fmt.Errorf("invalid container name")
	}
	expected, err := computeExpectedContainers(cfg)
	if err != nil {
		return nil, err
	}
	if expected[container] {
		return nil, fmt.Errorf("container %q is in the expected set — refuse to delete (use the per-tenant UI to stop it)", container)
	}
	cmd := fmt.Sprintf("docker rm -f %s", container)
	out, err := hostSSHExec(ctx, hc, cmd)
	if err != nil {
		return out, fmt.Errorf("docker rm -f %s: %w: %s", container, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
