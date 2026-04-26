package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ──────────────────────────────────────────────────────────────────────
// Tenant ops — per-domain on/off + per-container start/stop. Both
// operate on a tenant by its inventory slug; the host IP + SSH user
// come from `inventories/<env>/hosts.yml`.
// ──────────────────────────────────────────────────────────────────────

// tenantConn is the bare minimum needed to SSH to a tenant's host.
// Resolved from the inventory + filled in from cfg's defaults.
type tenantConn struct {
	Slug string
	Host string // ansible_host (IPv4)
	User string // ansible_user (default root)
	Port int    // ansible_port (default 22)
}

// resolveTenant pulls a tenant's connection details from
// hosts.yml. Errors when the slug isn't in the `clients` group — guards
// against arbitrary inventory targets being passed to handlers.
func resolveTenant(cfg *Config, slug string) (tenantConn, error) {
	tree, err := ReadInventoryTree(cfg.RepoPath, cfg.Env)
	if err != nil {
		return tenantConn{}, err
	}
	for _, h := range tree["clients"] {
		if h.Name == slug {
			return tenantConn{
				Slug: h.Name,
				Host: h.Host,
				User: h.User,
				Port: h.Port,
			}, nil
		}
	}
	return tenantConn{}, fmt.Errorf("tenant %q not found in clients group", slug)
}

// sshExec runs a single command on a tenant host via the runner's
// preinstalled SSH key (~/.ssh/id_ed25519). StrictHostKeyChecking is
// off — runner already runs ansible with HostKeyChecking=False, same
// trust model. Returns combined stdout+stderr or the command error.
func sshExec(ctx context.Context, t tenantConn, command string) ([]byte, error) {
	if t.Host == "" {
		return nil, errors.New("tenant has no ansible_host IP")
	}
	user := t.User
	if user == "" {
		user = "root"
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-p", fmt.Sprintf("%d", port),
		fmt.Sprintf("%s@%s", user, t.Host),
		command,
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "ssh", args...)
	return cmd.CombinedOutput()
}

// ──────────────────────────────────────────────────────────────────────
// Containers
// ──────────────────────────────────────────────────────────────────────

// ContainerInfo is the JSON shape returned by GET /api/tenants/<slug>/containers.
// Fields mirror what `docker ps -a --format '{{json .}}'` emits, trimmed
// to the columns the UI actually uses.
type ContainerInfo struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`   // running|exited|paused|...
	Status  string `json:"status"`  // human-friendly, includes uptime
	Running bool   `json:"running"` // derived: State == "running"
	Created string `json:"created"` // raw "CreatedAt" from docker
}

// dockerPSEntry mirrors the relevant fields out of `docker ps -a
// --format '{{json .}}'`. Fields are documented in `docker ps --help`.
type dockerPSEntry struct {
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	CreatedAt string `json:"CreatedAt"`
}

// ListTenantContainers SSHes to the tenant host and returns every
// container whose name matches the tenant's prefix (`keeppio-<slug>-…`).
// Sorted alphabetically for stable rendering.
func ListTenantContainers(ctx context.Context, cfg *Config, slug string) ([]ContainerInfo, error) {
	t, err := resolveTenant(cfg, slug)
	if err != nil {
		return nil, err
	}
	prefix := "keeppio-" + slug + "-"
	// `--format '{{json .}}'` emits one JSON object per line.
	cmd := fmt.Sprintf("docker ps -a --filter name=^/%s --format '{{json .}}'", prefix)
	out, err := sshExec(ctx, t, cmd)
	if err != nil {
		return nil, fmt.Errorf("ssh docker ps: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var containers []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var e dockerPSEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip malformed line; don't fail the whole list
		}
		// Defensive: docker may match keeppio-<slug>-* AND
		// keeppio-<slug>2-* with `name=` (substring); skip names that
		// don't actually start with the prefix.
		if !strings.HasPrefix(e.Names, prefix) {
			continue
		}
		containers = append(containers, ContainerInfo{
			Name:    e.Names,
			Image:   e.Image,
			State:   e.State,
			Status:  e.Status,
			Running: e.State == "running",
			Created: e.CreatedAt,
		})
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Name < containers[j].Name
	})
	return containers, nil
}

// ToggleTenantContainer SSHes and runs `docker start` or `docker stop`.
// Refuses if the container name doesn't match the tenant's prefix —
// the slug is the only auth boundary we have here, so we enforce it.
func ToggleTenantContainer(ctx context.Context, cfg *Config, slug, name string, start bool) ([]byte, error) {
	t, err := resolveTenant(cfg, slug)
	if err != nil {
		return nil, err
	}
	prefix := "keeppio-" + slug + "-"
	if !strings.HasPrefix(name, prefix) {
		return nil, fmt.Errorf("container name %q is not under tenant prefix %q", name, prefix)
	}
	// Reject anything with shell metacharacters; defense in depth even
	// though we already match the prefix.
	if strings.ContainsAny(name, "`;&|$<>\"'\\ \t\n") {
		return nil, fmt.Errorf("invalid container name")
	}
	op := "stop"
	if start {
		op = "start"
	}
	cmd := fmt.Sprintf("docker %s %s", op, name)
	out, err := sshExec(ctx, t, cmd)
	if err != nil {
		return out, fmt.Errorf("docker %s %s: %w: %s", op, name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ──────────────────────────────────────────────────────────────────────
// Per-domain disabled state
// ──────────────────────────────────────────────────────────────────────

// ReadDisabledDomains returns the list of FQDNs currently marked as
// disabled for a tenant. Source of truth is the committed file
// `inventories/<env>/host_vars/<slug>/disabled_domains.yml`. Missing
// file ⇒ empty slice (no error). The file is rewritten by the
// tenant-domain-toggle playbook on every change.
func ReadDisabledDomains(repo, env, slug string) ([]string, error) {
	path := filepath.Join(repo, "inventories", env, "host_vars", slug, "disabled_domains.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		DisabledDomains []string `yaml:"disabled_domains"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.DisabledDomains, nil
}

// IsDomainDisabled is a tiny helper: true iff `fqdn` is in the list of
// disabled domains for `slug`.
func IsDomainDisabled(repo, env, slug, fqdn string) bool {
	list, err := ReadDisabledDomains(repo, env, slug)
	if err != nil {
		return false
	}
	for _, d := range list {
		if d == fqdn {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────
// Per-container disabled state
// ──────────────────────────────────────────────────────────────────────

// ReadDisabledContainers returns the list of container names currently
// marked as disabled for a tenant. Source of truth is the committed
// file `inventories/<env>/host_vars/<slug>/disabled_containers.yml`.
// Missing file ⇒ empty slice (no error). The file is rewritten by
// the tenant-container-toggle playbook on every change; the apps role
// re-applies it on every provision/deploy so the state survives
// `compose up -d` recreations.
func ReadDisabledContainers(repo, env, slug string) ([]string, error) {
	path := filepath.Join(repo, "inventories", env, "host_vars", slug, "disabled_containers.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		DisabledContainers []string `yaml:"disabled_containers"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.DisabledContainers, nil
}
