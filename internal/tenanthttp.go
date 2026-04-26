package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ──────────────────────────────────────────────────────────────────────
// HTTP handlers for tenant operations: list / start / stop containers,
// list / toggle disabled domains. Two entry points routed by path:
//
//   /api/tenants/<slug>/containers                  GET
//   /api/tenants/<slug>/containers/<name>/toggle    POST  body {"start": bool}
//   /api/tenants/<slug>/domains                     GET   → {disabled:[...]}
//   /api/tenants/<slug>/domains/toggle              POST  body {"domain_key":"webapp","state":"on|off"}
//
// `/ui/tenants/...` mirrors the same routes 1:1 but auth's via the
// session cookie, so the inventory detail page can call them without
// minting a bearer token in JS. Both share `dispatchTenants()`.
// ──────────────────────────────────────────────────────────────────────

// handleAPITenantsRoute is the bearer-token entry. requireAPIAuth has
// already validated the token before this runs.
func (s *Server) handleAPITenantsRoute(w http.ResponseWriter, r *http.Request) {
	s.dispatchTenants(w, r, "/api/tenants/", "api:"+currentAPITokenName(r))
}

// handleUITenantsRoute is the cookie-auth entry, used by the page's
// inline JS. Same handlers, same JSON shapes.
func (s *Server) handleUITenantsRoute(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == "" {
		user = "ui"
	}
	s.dispatchTenants(w, r, "/ui/tenants/", user)
}

// dispatchTenants splits the URL path into <slug>/<rest> and routes to
// the right sub-handler. `who` is what gets recorded in the audit row
// when an action gets enqueued (so we can tell apart UI clicks vs API
// calls in /tasks).
func (s *Server) dispatchTenants(w http.ResponseWriter, r *http.Request, prefix, who string) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 2 || parts[0] == "" {
		apiErr(w, http.StatusNotFound, "expected <slug>/<resource>")
		return
	}
	slug, resource := parts[0], parts[1]

	switch resource {
	case "containers":
		// /<slug>/containers              → list
		// /<slug>/containers/<name>/toggle → toggle
		if len(parts) == 2 {
			s.handleTenantContainersList(w, r, slug)
			return
		}
		if len(parts) == 4 && parts[3] == "toggle" {
			s.handleTenantContainerToggle(w, r, slug, parts[2])
			return
		}
		apiErr(w, http.StatusNotFound, "unknown containers route")

	case "domains":
		// /<slug>/domains       GET  → list disabled
		// /<slug>/domains/toggle POST → enqueue toggle
		if len(parts) == 2 {
			s.handleTenantDomainsList(w, r, slug)
			return
		}
		if len(parts) == 3 && parts[2] == "toggle" {
			s.handleTenantDomainToggle(w, r, slug, who)
			return
		}
		apiErr(w, http.StatusNotFound, "unknown domains route")

	default:
		apiErr(w, http.StatusNotFound, "unknown tenant resource: "+resource)
	}
}

// ──────────────────────────────────────────────────────────────────────
// Containers
// ──────────────────────────────────────────────────────────────────────

func (s *Server) handleTenantContainersList(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	out, err := ListTenantContainers(r.Context(), s.cfg, slug)
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	apiJSON(w, map[string]any{
		"slug":       slug,
		"containers": out,
	})
}

func (s *Server) handleTenantContainerToggle(w http.ResponseWriter, r *http.Request, slug, name string) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		Start bool `json:"start"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4*1024)).Decode(&body); err != nil {
		// Default to false if no body — caller can also send ?start=1.
		if v := r.URL.Query().Get("start"); v == "1" || v == "true" {
			body.Start = true
		}
	}
	out, err := ToggleTenantContainer(r.Context(), s.cfg, slug, name, body.Start)
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	apiJSON(w, map[string]any{
		"slug":    slug,
		"name":    name,
		"started": body.Start,
		"output":  strings.TrimSpace(string(out)),
	})
}

// ──────────────────────────────────────────────────────────────────────
// Domains
// ──────────────────────────────────────────────────────────────────────

func (s *Server) handleTenantDomainsList(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	list, err := ReadDisabledDomains(s.cfg.RepoPath, s.cfg.Env, slug)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []string{}
	}
	apiJSON(w, map[string]any{
		"slug":     slug,
		"disabled": list,
	})
}

// handleTenantDomainToggle enqueues the `tenant-domain-toggle`
// playbook. Returns the task id so the UI can link to /tasks/<id>.
// The playbook persists the desired state (commits it to the repo)
// and applies the nginx role on the tenant host — the same code path
// regular operators take via the run form.
func (s *Server) handleTenantDomainToggle(w http.ResponseWriter, r *http.Request, slug, who string) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		DomainKey string `json:"domain_key"`
		State     string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4*1024)).Decode(&body); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	switch body.DomainKey {
	case "webapp", "api", "bridge", "paynl", "reverb":
	default:
		apiErr(w, http.StatusBadRequest, "domain_key must be one of webapp|api|bridge|paynl|reverb")
		return
	}
	if body.State != "on" && body.State != "off" {
		apiErr(w, http.StatusBadRequest, "state must be on|off")
		return
	}
	action, ok := s.cat.ByID("tenant-domain-toggle")
	if !ok {
		apiErr(w, http.StatusInternalServerError, "tenant-domain-toggle action not in catalog (pull repo?)")
		return
	}
	args := map[string]string{
		"tenant":     slug,
		"domain_key": body.DomainKey,
		"state":      body.State,
	}
	taskID, err := s.runner.Enqueue(r.Context(), action, args, who)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_id": taskID,
		"url":     fmt.Sprintf("/tasks/%d", taskID),
	})
}
