package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────────────
// Per-tenant status fetch. Each tenant has its own Uptime Kuma
// container on the ops host (named keeppio-kuma-<slug>). The runner
// reaches Kuma via SSH → docker exec the curl/wget that's already in
// the Kuma image, hitting Kuma's PUBLIC status-page API which doesn't
// need auth.
//
// Two endpoints are used:
//   /api/status-page/<slug>             metadata + monitor list
//   /api/status-page/heartbeat/<slug>   per-monitor heartbeat history
//
// Returned summary collapses both into a small JSON shape that's easy
// to render as a badge in the UI.
// ──────────────────────────────────────────────────────────────────────

// StatusSummary is the shape returned by ListTenantStatus / one entry
// in BatchTenantStatus. Field names mirror the runner's existing JSON
// conventions (snake_case).
type StatusSummary struct {
	Slug      string           `json:"slug"`
	Up        int              `json:"up"`
	Down      int              `json:"down"`
	Pending   int              `json:"pending"`
	Total     int              `json:"total"`
	Uptime24h *float64         `json:"uptime_24h,omitempty"` // 0..1 fraction; nil when unknown
	Monitors  []MonitorStatus  `json:"monitors,omitempty"`
	Error     string           `json:"error,omitempty"` // populated when fetch failed; rest empty
	StatusURL string           `json:"status_url"`      // public page link
	FetchedAt time.Time        `json:"fetched_at"`
}

// MonitorStatus is one monitor's current up/down state. We expose the
// FQDN (Kuma's monitor name) and the most recent heartbeat status:
//   1 = up, 0 = down, 2 = pending, 3 = maintenance.
type MonitorStatus struct {
	Name   string `json:"name"`
	Status int    `json:"status"`
}

// kumaStatusPage is the public /api/status-page/<slug> response shape.
// We only deserialise the fields we need; Kuma adds others for theming
// etc.
type kumaStatusPage struct {
	StatusPage struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
	} `json:"statusPage"`
	PublicGroupList []struct {
		MonitorList []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"monitorList"`
	} `json:"publicGroupList"`
}

// kumaHeartbeats is the /api/status-page/heartbeat/<slug> response.
// Both maps are keyed by monitor ID (as a string).
type kumaHeartbeats struct {
	HeartbeatList map[string][]struct {
		Status int     `json:"status"`
		Time   string  `json:"time"`
		Ping   *int    `json:"ping"`
	} `json:"heartbeatList"`
	UptimeList map[string]float64 `json:"uptimeList"`
}

// resolveOpsHost returns connection info for the env's ops host. The
// tenant's status page is fronted by THIS host's nginx, regardless of
// which VPS the tenant's apps live on.
func resolveOpsHost(cfg *Config) (tenantConn, error) {
	tree, err := ReadInventoryTree(cfg.RepoPath, cfg.Env)
	if err != nil {
		return tenantConn{}, err
	}
	ops := tree["ops"]
	if len(ops) == 0 {
		return tenantConn{}, errors.New("no hosts in `ops` group")
	}
	// First (alphabetical, ReadInventoryTree sorts by name) — matches
	// tenant-onboard's `groups['ops'] | first` choice.
	h := ops[0]
	return tenantConn{Slug: h.Name, Host: h.Host, User: h.User, Port: h.Port}, nil
}

// FetchTenantStatus pulls one tenant's Kuma summary. Best-effort: any
// error (SSH, network, Kuma not yet seeded) is captured into the
// returned summary's Error field rather than bubbled — the UI shows
// a small grey "?" badge in that case instead of breaking the page.
func FetchTenantStatus(ctx context.Context, cfg *Config, slug, webappFqdn string) StatusSummary {
	out := StatusSummary{
		Slug:      slug,
		FetchedAt: time.Now().UTC(),
		StatusURL: "https://status." + webappFqdn + "/status/" + slug,
	}
	ops, err := resolveOpsHost(cfg)
	if err != nil {
		out.Error = err.Error()
		return out
	}

	// Run two curls inside the tenant's Kuma container so we don't
	// need to know the host port assignment (which is computed at
	// apply time by the role). The Kuma image ships busybox wget;
	// it's a hard dependency of the upstream Dockerfile.
	page, err := kumaCall(ctx, ops, slug, "/api/status-page/"+slug)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	beats, beatsErr := kumaCall(ctx, ops, slug, "/api/status-page/heartbeat/"+slug)
	// beatsErr is non-fatal — when no heartbeats have been recorded
	// yet (fresh seed) the endpoint can 404 briefly. Carry on with
	// counts only.

	var pageDoc kumaStatusPage
	if err := json.Unmarshal(page, &pageDoc); err != nil {
		out.Error = "parse status-page: " + err.Error()
		return out
	}
	var beatsDoc kumaHeartbeats
	if beatsErr == nil {
		_ = json.Unmarshal(beats, &beatsDoc)
	}

	type idName struct {
		id   int
		name string
	}
	var monitors []idName
	for _, g := range pageDoc.PublicGroupList {
		for _, m := range g.MonitorList {
			monitors = append(monitors, idName{m.ID, m.Name})
		}
	}
	out.Total = len(monitors)

	// Sum uptime over monitors (24h key). Kuma exposes uptime under
	// keys "<id>_24" and "<id>_720" (24h and 30d). We use 24h.
	var uptimeSum float64
	var uptimeCount int
	for _, m := range monitors {
		hb := beatsDoc.HeartbeatList[fmt.Sprint(m.id)]
		ms := MonitorStatus{Name: m.name}
		if len(hb) > 0 {
			ms.Status = hb[len(hb)-1].Status
			switch ms.Status {
			case 1:
				out.Up++
			case 0:
				out.Down++
			case 2, 3:
				out.Pending++
			}
		} else {
			out.Pending++
			ms.Status = 2
		}
		out.Monitors = append(out.Monitors, ms)
		if u, ok := beatsDoc.UptimeList[fmt.Sprintf("%d_24", m.id)]; ok {
			uptimeSum += u
			uptimeCount++
		}
	}
	if uptimeCount > 0 {
		avg := uptimeSum / float64(uptimeCount)
		out.Uptime24h = &avg
	}
	return out
}

// kumaCall runs `curl http://localhost:3001<path>` inside the tenant's
// Kuma container via SSH → docker exec. Returns the raw body bytes or
// an error including stderr context. Uses sshExecQuiet so SSH's
// "Warning: Permanently added ..." line doesn't end up corrupting the
// JSON response (sshExec uses CombinedOutput which would mix stderr).
func kumaCall(ctx context.Context, ops tenantConn, slug, path string) ([]byte, error) {
	if !isSafeKumaPath(path) {
		return nil, fmt.Errorf("unsafe path: %q", path)
	}
	if !isSafeSlug(slug) {
		return nil, fmt.Errorf("unsafe slug: %q", slug)
	}
	// docker exec ... curl. The Kuma upstream image (debian-slim base)
	// ships curl, not wget. -fsSm5: fail on HTTP error (-f), silent
	// (-s), still emit errors on stderr (-S), 5s max time (-m 5).
	cmd := fmt.Sprintf(
		"docker exec keeppio-kuma-%s curl -fsSm5 http://localhost:3001%s",
		slug, path,
	)
	out, stderr, err := sshExecQuiet(ctx, ops, cmd)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("ssh kuma %s%s: %w: %s", slug, path, err, msg)
	}
	return out, nil
}

// sshExecQuiet is sshExec's sibling that returns stdout and stderr
// separately, AND adds -o LogLevel=ERROR so SSH's host-key-additions
// warning doesn't show up at all. Used when stdout is consumed as
// structured data (JSON) — mixing in stderr would corrupt parsing.
func sshExecQuiet(ctx context.Context, t tenantConn, command string) ([]byte, []byte, error) {
	if t.Host == "" {
		return nil, nil, errors.New("tenant has no ansible_host IP")
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
		"-o", "LogLevel=ERROR",
		"-p", fmt.Sprintf("%d", port),
		fmt.Sprintf("%s@%s", user, t.Host),
		command,
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// isSafeKumaPath gates the command we ssh+exec — we ONLY ever talk to
// /api/status-page/* and /api/status-page/heartbeat/*. The slug is
// already constrained to [a-z0-9-]+ by tenant-onboard, but defence in
// depth never hurts.
func isSafeKumaPath(p string) bool {
	if !strings.HasPrefix(p, "/api/status-page/") {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '/' || r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func isSafeSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// BatchTenantStatus pulls status for every tenant in this env's
// `clients` group, in parallel. Used by the inventory home page to
// show a per-tenant up/down dot. Failures populate per-tenant `error`
// fields rather than aborting the batch — one offline tenant doesn't
// black out the whole dashboard.
func BatchTenantStatus(ctx context.Context, cfg *Config) []StatusSummary {
	tree, err := ReadInventoryTree(cfg.RepoPath, cfg.Env)
	if err != nil {
		return []StatusSummary{{Slug: "_inventory", Error: err.Error()}}
	}
	clients := tree["clients"]
	if len(clients) == 0 {
		return []StatusSummary{}
	}

	// Concurrency cap. SSH is the bottleneck — too many parallel ssh
	// connections trip ops01's MaxStartups. 4 is comfortably under the
	// sshd default of 10:30:100.
	const concurrency = 4
	sem := make(chan struct{}, concurrency)
	results := make([]StatusSummary, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		i, c := i, c
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fqdn := c.PrimaryFqdn
			if fqdn == "" {
				fqdn = c.Name
			}
			results[i] = FetchTenantStatus(ctx, cfg, c.Name, fqdn)
		}()
	}
	wg.Wait()
	return results
}
