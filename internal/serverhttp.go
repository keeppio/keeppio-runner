package internal

import (
	"net/http"
	"strings"
	"time"
)

// ──────────────────────────────────────────────────────────────────────
// HTTP handlers for host-scoped (server / ops) operations. Today this
// is just the orphan-container view; future host-level features (disk
// usage, system load, log tails …) plug in here under the same prefix.
//
// Routes (mirrored under /api/servers/ for bearer auth):
//
//   GET  /ui/servers/<name>/orphan-containers
//   POST /ui/servers/<name>/orphan-containers/<container>/delete
// ──────────────────────────────────────────────────────────────────────

// handleAPIServersRoute is the bearer-token entry point.
func (s *Server) handleAPIServersRoute(w http.ResponseWriter, r *http.Request) {
	s.dispatchServers(w, r, "/api/servers/", false)
}

// handleUIServersRoute is the cookie-auth entry point used by the
// inventory detail page's vanilla-fetch JS.
func (s *Server) handleUIServersRoute(w http.ResponseWriter, r *http.Request) {
	s.dispatchServers(w, r, "/ui/servers/", true)
}

// dispatchServers routes /<prefix>/<name>/<resource>/[...] requests.
// `name` is the inventory host name; resolveHost() upstream rejects
// anything that's not in the `servers` or `ops` group. `cached` is
// true for cookie-auth UI calls — orphan list reads short-circuit
// through s.state with a 30 s TTL; bearer-auth API calls always go
// to SSH for live state.
func (s *Server) dispatchServers(w http.ResponseWriter, r *http.Request, prefix string, cached bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 2 || parts[0] == "" {
		apiErr(w, http.StatusNotFound, "expected <name>/<resource>")
		return
	}
	name, resource := parts[0], parts[1]
	switch resource {
	case "orphan-containers":
		// /<name>/orphan-containers                    GET    list
		// /<name>/orphan-containers/<container>/delete POST   docker rm -f
		if len(parts) == 2 {
			s.handleOrphanContainersList(w, r, name, cached)
			return
		}
		if len(parts) == 4 && parts[3] == "delete" {
			s.state.Invalidate("orphans:" + name)
			s.handleOrphanContainerDelete(w, r, name, parts[2])
			return
		}
		apiErr(w, http.StatusNotFound, "unknown orphan-containers route")
	default:
		apiErr(w, http.StatusNotFound, "unknown server resource: "+resource)
	}
}

func (s *Server) handleOrphanContainersList(w http.ResponseWriter, r *http.Request, name string, cached bool) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	fresh := r.URL.Query().Get("fresh") == "1"
	cacheKey := "orphans:" + name
	if cached && !fresh {
		if v, at, ok := s.state.Get(cacheKey); ok {
			apiJSON(w, map[string]any{
				"server":       name,
				"containers":   v,
				"fetched_at":   at.UnixMilli(),
				"cache_age_ms": time.Since(at).Milliseconds(),
				"from_cache":   true,
			})
			return
		}
	}
	out, err := ListOrphanContainers(r.Context(), s.cfg, name)
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if out == nil {
		out = []ContainerInfo{}
	}
	at := time.Now()
	if cached {
		at = s.state.Set(cacheKey, out)
	}
	apiJSON(w, map[string]any{
		"server":       name,
		"containers":   out,
		"fetched_at":   at.UnixMilli(),
		"cache_age_ms": 0,
		"from_cache":   false,
	})
}

func (s *Server) handleOrphanContainerDelete(w http.ResponseWriter, r *http.Request, name, container string) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	out, err := DeleteOrphanContainer(r.Context(), s.cfg, name, container)
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	apiJSON(w, map[string]any{
		"server": name,
		"name":   container,
		"output": strings.TrimSpace(string(out)),
	})
}
