package internal

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const sessionCookie = "kr_session"

// Server wires the dependency graph and exposes ServeMux. Methods on
// it are HTTP handlers; `mux()` returns the routed mux.
type Server struct {
	cfg      *Config
	db       *sql.DB
	cat      *Catalog
	runner   *Runner
	tmpl     *template.Template
	staticFS embed.FS
	upgr     websocket.Upgrader
	state    *StateCache // 30s TTL cache for UI-side container/orphan reads
}

func NewServer(cfg *Config, db *sql.DB, cat *Catalog, runner *Runner, tplFS, staticFS embed.FS) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"timeAgo": timeAgo,
		"upper":   strings.ToUpper,
		"statusBadge": func(s string) string {
			switch s {
			case "success":
				return "bg-green-100 text-green-800"
			case "running":
				return "bg-blue-100 text-blue-800 animate-pulse"
			case "queued":
				return "bg-zinc-100 text-zinc-800"
			default:
				return "bg-red-100 text-red-800"
			}
		},
		"severityClass": func(s string) string {
			if s == "danger" {
				return "border-red-300 hover:bg-red-50"
			}
			return "border-zinc-200 hover:bg-zinc-50"
		},
		"jsonPretty": func(s string) string {
			var v any
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				return s
			}
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
		// Tasks page helpers — kept here so templates stay free of
		// per-row Go calls. See tasksHelpers.go for implementations.
		"taskScope":    cat.TaskScope,
		"taskDuration": taskDuration,
		"deref":        derefTime,
		"pageURLBase":  pageURLBase,
		"add":          func(a, b int) int { return a + b },
		"sub":          func(a, b int) int { return a - b },
	}).ParseFS(tplFS, "templates/*.html", "templates/views/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		db:       db,
		cat:      cat,
		runner:   runner,
		tmpl:     tmpl,
		staticFS: staticFS,
		state:    NewStateCache(30 * time.Second),
		upgr: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}, nil
}

func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	// Health check, no auth. External monitors (Kuma on the same ops
	// box, third-party uptime, etc.) ping this; the server up + the
	// SQLite handle alive is enough signal that the runner can
	// accept new tasks.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "db ping failed: "+err.Error()+"\n")
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	// Embedded static assets (background image, etc.) served from /static/<file>.
	mux.Handle("/static/", http.StripPrefix("/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("/login", s.handleLoginGet)
	mux.HandleFunc("/login.submit", s.handleLoginPost)
	mux.HandleFunc("/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("/", s.requireAuth(s.handleHomeRedirect))
	mux.HandleFunc("/r/", s.requireAuth(s.handleResource)) // env root + every resource view
	mux.HandleFunc("/run/", s.requireAuth(s.handleRun))                  // GET: form, POST: enqueue
	mux.HandleFunc("/tasks", s.requireAuth(s.handleTasksList))
	mux.HandleFunc("/tasks/", s.requireAuth(s.handleTaskShow))           // /tasks/<id>
	mux.HandleFunc("/tasks/cancel/", s.requireAuth(s.handleTaskCancel))  // POST
	mux.HandleFunc("/tasks/replay/", s.requireAuth(s.handleTaskReplay))  // POST
	// Legacy inventory routes — 302 to the new /r/... equivalents.
	mux.HandleFunc("/inventory", s.requireAuth(s.handleLegacyInventoryRedirect))
	mux.HandleFunc("/inventory/", s.requireAuth(s.handleLegacyInventoryRedirect))
	mux.HandleFunc("/settings", s.requireAuth(s.handleSettingsGet))
	mux.HandleFunc("/settings.submit", s.requireAuth(s.handleSettingsPost))
	mux.HandleFunc("/settings/pull-repo", s.requireAuth(s.handleSettingsPullRepo))
	mux.HandleFunc("/settings/api-tokens", s.requireAuth(s.handleAPITokenCreate))   // POST: create
	mux.HandleFunc("/settings/api-tokens/revoke", s.requireAuth(s.handleAPITokenRevoke)) // POST: revoke
	mux.HandleFunc("/ws/tasks/", s.requireAuth(s.handleTaskWS))          // /ws/tasks/<id>

	// Bottom-console JSON helpers (cookie auth — same realm as the UI).
	mux.HandleFunc("/ui/console/recent", s.requireAuth(s.handleConsoleRecent))
	mux.HandleFunc("/ui/catalog", s.requireAuth(s.handleCatalogJSON))
	mux.HandleFunc("/ui/tasks/", s.requireAuth(s.handleUITaskLog)) // /ui/tasks/<id>/log

	// JSON API for external observers + automation. Auth: Authorization:
	// Bearer kr_pat_…  (token issued from /settings). Reads are GETs;
	// the one write endpoint is /api/runs (POST) — same code path as the
	// UI's run form, same field validation, same Enqueue. Cancel/state-
	// change endpoints stay out for now (the UI is fine for those).
	mux.HandleFunc("/api/tasks", s.requireAPIAuth(s.handleAPITasksList))
	mux.HandleFunc("/api/tasks/", s.requireAPIAuth(s.handleAPITask))
	mux.HandleFunc("/api/runs", s.requireAPIAuth(s.handleAPIRunCreate))

	// Tenant ops API — bearer-token auth, JSON in/out. Used by the
	// inventory detail page (via the cookie-auth aliases below) and by
	// any external automation.
	mux.HandleFunc("/api/tenants/", s.requireAPIAuth(s.handleAPITenantsRoute))

	// Cookie-auth equivalents so the inventory page's vanilla-fetch
	// JS can hit them without exposing a bearer token to the browser.
	// Exact same handlers under a different prefix.
	mux.HandleFunc("/ui/tenants/", s.requireAuth(s.handleUITenantsRoute))

	// Host-scoped ops (orphan containers on registered servers / ops
	// boxes). Same shape as the tenants routes — bearer-auth API +
	// cookie-auth UI alias for the inventory page's inline JS.
	mux.HandleFunc("/api/servers/", s.requireAPIAuth(s.handleAPIServersRoute))
	mux.HandleFunc("/ui/servers/", s.requireAuth(s.handleUIServersRoute))
	return mux
}

// ──────────────────────────────────────────────────────────────────────
// Auth middleware + login
// ──────────────────────────────────────────────────────────────────────

type ctxKey int

const userKey ctxKey = 0

// resolvedField is what the run form template iterates over. Used in
// both the GET-form path and the POST-validation-error re-render so
// the template can rely on a single shape (esp. .Value).
type resolvedField struct {
	Field
	Options []string
	Value   string
	Error   string
}

func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		user, err := LookupSession(r.Context(), s.db, c.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		h(w, r.WithContext(ctx))
	}
}

func currentUser(r *http.Request) string {
	if u, ok := r.Context().Value(userKey).(string); ok {
		return u
	}
	return ""
}

func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Env": s.cfg.Env})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	password := r.FormValue("password")
	wantUser, err := GetAdminUsername(r.Context(), s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if username != wantUser {
		s.render(w, "login.html", map[string]any{"Env": s.cfg.Env, "Error": "invalid credentials"})
		return
	}
	ok, err := VerifyAdminPassword(r.Context(), s.db, password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		s.render(w, "login.html", map[string]any{"Env": s.cfg.Env, "Error": "invalid credentials"})
		return
	}
	id, err := NewSession(r.Context(), s.db, wantUser)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.HasPrefix(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = DeleteSession(r.Context(), s.db, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ──────────────────────────────────────────────────────────────────────
// Action form (dashboard rendering moved to /r/... in resourcehttp.go)
// ──────────────────────────────────────────────────────────────────────

// /run/<id>  GET = form, POST = enqueue (POSTed to /run/<id>.submit
// to keep the GET clean for permalink-friendly URLs). Query params
// are read as field-value defaults — used by the inventory drill-down
// page to land you on a pre-filled form.
//
// Modal mode: when `?modal=1` is in the query string OR the request is
// an HTMX swap (`HX-Request` header set by /static/vendor/htmx.min.js),
// the response is a fragment (`run-fragment.html`) instead of a full
// page, suitable for swapping into the modal mount in `_layout.html`.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/run/")
	id = strings.TrimSuffix(id, ".submit")
	action, ok := s.cat.ByID(id)
	if !ok {
		http.Error(w, "no such action", http.StatusNotFound)
		return
	}
	isModal := r.URL.Query().Get("modal") == "1" || r.Header.Get("HX-Request") == "true"
	if r.Method == http.MethodPost {
		s.handleRunSubmit(w, r, action, isModal)
		return
	}
	q := r.URL.Query()
	fields := make([]resolvedField, 0, len(action.Fields))
	for _, f := range action.Fields {
		rf := resolvedField{Field: f, Value: q.Get(f.Name)}
		if f.Type == "enum" && f.Source != nil {
			opts, err := ResolveOptions(r.Context(), s.cfg, f)
			if err != nil {
				rf.Error = err.Error()
			} else {
				rf.Options = opts
			}
		}
		fields = append(fields, rf)
	}
	confirmToken, confirmLabel := computeConfirmToken(action, q)
	data := map[string]any{
		"Env":          s.cfg.Env,
		"User":         currentUser(r),
		"Action":       action,
		"Fields":       fields,
		"DeployPubkey": deployPubkeyFor(action, s.cfg),
		"ConfirmToken": confirmToken,
		"ConfirmLabel": confirmLabel,
		"Modal":        isModal,
	}
	if isModal {
		s.render(w, "run-fragment.html", data)
		return
	}
	s.render(w, "run.html", data)
}

// computeConfirmToken returns the typed-confirm token for danger actions.
// Strategy: pick the first applies_to.field with a value in the current
// query string — that's the resource the operator launched the action
// against, and typing its slug is a strong "I really mean this resource"
// signal. Returns ("", "") for non-danger actions.
func computeConfirmToken(action Action, q url.Values) (token, fieldLabel string) {
	if action.Severity != "danger" {
		return "", ""
	}
	for _, ap := range action.AppliesTo {
		if ap.Field == "" {
			continue
		}
		v := q.Get(ap.Field)
		if v == "" {
			continue
		}
		for _, f := range action.Fields {
			if f.Name == ap.Field {
				fieldLabel = f.Label
				break
			}
		}
		if fieldLabel == "" {
			fieldLabel = ap.Field
		}
		return v, fieldLabel
	}
	// No prefill — fall back to the action ID itself.
	return action.ID, "action id"
}

// deployPubkeyFor returns the env's deploy SSH public key when the
// action is one whose operator workflow needs it (registering a fresh
// server, where the key has to be installed before the runner can SSH
// in). Returns "" otherwise so the template can stay simple.
func deployPubkeyFor(a Action, cfg *Config) string {
	if a.ID != "server-register" {
		return ""
	}
	return ReadDeployPubkey(cfg.RepoPath, cfg.Env)
}

func (s *Server) handleRunSubmit(w http.ResponseWriter, r *http.Request, action Action, isModal bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args, err := ValidateSubmission(action, r.PostForm)
	if err != nil {
		// Re-render the form with the error + previously-entered values.
		fields := make([]resolvedField, 0, len(action.Fields))
		for _, f := range action.Fields {
			rf := resolvedField{Field: f, Value: r.FormValue(f.Name)}
			if f.Type == "enum" && f.Source != nil {
				opts, sErr := ResolveOptions(r.Context(), s.cfg, f)
				if sErr != nil {
					rf.Error = sErr.Error()
				} else {
					rf.Options = opts
				}
			}
			fields = append(fields, rf)
		}
		// Carry over the confirm-token context so the typed-confirm gate
		// re-renders correctly on validation error.
		confirmToken, confirmLabel := computeConfirmToken(action, r.PostForm)
		data := map[string]any{
			"Env":          s.cfg.Env,
			"User":         currentUser(r),
			"Action":       action,
			"Fields":       fields,
			"Error":        err.Error(),
			"ConfirmToken": confirmToken,
			"ConfirmLabel": confirmLabel,
			"Modal":        isModal,
		}
		if isModal {
			// 422 lets HTMX swap the fragment back into the modal body
			// while still flagging "this didn't go through".
			w.WriteHeader(http.StatusUnprocessableEntity)
			s.render(w, "run-fragment.html", data)
			return
		}
		s.render(w, "run.html", data)
		return
	}
	taskID, err := s.runner.Enqueue(r.Context(), action, args, currentUser(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if isModal {
		// HTMX consumers: emit a custom event the bottom console listens
		// for. JSON payload carries the new task id so the console can
		// open its WebSocket without an extra round-trip. Empty body —
		// the modal closes itself on receipt of the event.
		scope := scopeFromArgs(action, args)
		payload, _ := json.Marshal(map[string]any{
			"task_id":      taskID,
			"action_id":    action.ID,
			"action_label": action.Label,
			"scope":        scope,
		})
		w.Header().Set("HX-Trigger", `{"keeppio:task-started":` + string(payload) + `}`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<div class="muted" style="padding:14px;font-size:12px">Task #` + strconv.FormatInt(taskID, 10) + ` queued.</div>`))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", taskID), http.StatusSeeOther)
}

// scopeFromArgs picks a human-friendly scope chip for the bottom console.
// Uses the first non-secret applies_to field's value (e.g. "tenant=demo"
// on tenant-deploy → "demo"). Falls back to the action ID when no field
// reveals a useful scope.
func scopeFromArgs(action Action, args map[string]string) string {
	for _, ap := range action.AppliesTo {
		if v := args[ap.Field]; v != "" {
			return v
		}
	}
	return action.ID
}

// ──────────────────────────────────────────────────────────────────────
// Task list + detail + cancel
// ──────────────────────────────────────────────────────────────────────

type taskRow struct {
	ID          int64
	ActionID    string
	ActionLabel string
	Status      string
	ArgsJSON    string
	CommitHash  string
	Username    string
	CreatedAt   time.Time
	StartedAt   *time.Time
	EndedAt     *time.Time
	ExitCode    *int
}

// taskFilters captures the (validated) query-string parameters that
// shape the tasks listing.
type taskFilters struct {
	Page     int
	PageSize int
	Status   string
	ActionID string
	Search   string
}

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := taskFilters{
		Status:   strings.ToLower(strings.TrimSpace(q.Get("status"))),
		ActionID: strings.TrimSpace(q.Get("action")),
		Search:   strings.TrimSpace(q.Get("q")),
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p >= 1 {
		f.Page = p
	} else {
		f.Page = 1
	}
	if ps, err := strconv.Atoi(q.Get("page_size")); err == nil && ps > 0 {
		if ps > 200 {
			ps = 200
		}
		f.PageSize = ps
	} else {
		f.PageSize = 25
	}
	allowedStatus := map[string]bool{"": true, "queued": true, "running": true, "success": true, "error": true, "cancelled": true}
	if !allowedStatus[f.Status] {
		f.Status = ""
	}

	where := []string{}
	args := []any{}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.ActionID != "" {
		where = append(where, "action_id = ?")
		args = append(args, f.ActionID)
	}
	if f.Search != "" {
		// Match against action label, username, or args_json — keeps the
		// search box useful without dragging in FTS infra.
		where = append(where, "(action_label LIKE ? OR username LIKE ? OR args_json LIKE ?)")
		needle := "%" + f.Search + "%"
		args = append(args, needle, needle, needle)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM tasks"+whereSQL, args...).Scan(&total); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	offset := (f.Page - 1) * f.PageSize
	listArgs := append([]any{}, args...)
	listArgs = append(listArgs, f.PageSize, offset)
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, action_id, action_label, status, args_json, COALESCE(commit_hash,''), username,
		        created_at, started_at, ended_at, exit_code
		 FROM tasks`+whereSQL+
			` ORDER BY id DESC LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var tasks []taskRow
	for rows.Next() {
		var t taskRow
		if err := rows.Scan(&t.ID, &t.ActionID, &t.ActionLabel, &t.Status, &t.ArgsJSON, &t.CommitHash, &t.Username,
			&t.CreatedAt, &t.StartedAt, &t.EndedAt, &t.ExitCode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tasks = append(tasks, t)
	}

	totalPages := (total + f.PageSize - 1) / f.PageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if f.Page > totalPages {
		f.Page = totalPages
	}

	// Distinct action ids for the filter dropdown — drawn from the DB so
	// the list stays useful even when the catalog has dropped an action
	// that has historical task rows.
	actionIDs := []string{}
	idRows, err := s.db.QueryContext(r.Context(),
		`SELECT DISTINCT action_id FROM tasks ORDER BY action_id`)
	if err == nil {
		defer idRows.Close()
		for idRows.Next() {
			var id string
			if err := idRows.Scan(&id); err == nil {
				actionIDs = append(actionIDs, id)
			}
		}
	}

	s.render(w, "tasks.html", map[string]any{
		"Env":         s.cfg.Env,
		"User":        currentUser(r),
		"NavSection":  "tasks",
		"Tasks":       tasks,
		"Filters":     f,
		"Total":       total,
		"TotalPages":  totalPages,
		"ActionIDs":   actionIDs,
	})
}

func (s *Server) handleTaskShow(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	t, err := s.loadTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "no such task", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// On terminal status, serve the log file as the body. On running,
	// serve a placeholder + WS connect; the page tails live via JS.
	logBody := ""
	if t.LogPath != "" {
		if b, err := os.ReadFile(t.LogPath); err == nil {
			logBody = string(b)
		}
	}
	s.render(w, "task.html", map[string]any{
		"Env":     s.cfg.Env,
		"User":    currentUser(r),
		"Task":    t,
		"LogBody": logBody,
		"Live":    t.Status == "running" || t.Status == "queued",
	})
}

type taskDetail struct {
	taskRow
	LogPath string
}

func (s *Server) loadTask(ctx context.Context, id int64) (taskDetail, error) {
	var (
		t       taskDetail
		logPath string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, action_label, status, args_json, COALESCE(commit_hash,''), username, created_at, started_at, ended_at, exit_code, COALESCE(log_path,'')
		 FROM tasks WHERE id=?`, id).
		Scan(&t.ID, &t.ActionLabel, &t.Status, &t.ArgsJSON, &t.CommitHash, &t.Username, &t.CreatedAt, &t.StartedAt, &t.EndedAt, &t.ExitCode, &logPath)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.LogPath = logPath
	return t, nil
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/cancel/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	s.runner.Cancel(id)
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", id), http.StatusSeeOther)
}

// handleTaskReplay re-enqueues an existing task with the same action_id +
// args. Refuses while the source task is still queued / running so the
// operator can't double-fire by accident. Redirects to the new task's
// detail page so the live log is one click away.
func (s *Server) handleTaskReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/replay/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var (
		actionID, argsJSON, status string
	)
	err = s.db.QueryRowContext(r.Context(),
		`SELECT action_id, args_json, status FROM tasks WHERE id=?`, id).
		Scan(&actionID, &argsJSON, &status)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such task", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status == "queued" || status == "running" {
		http.Error(w, "refusing to replay a "+status+" task — wait for it to finish or cancel it", http.StatusConflict)
		return
	}
	action, ok := s.cat.ByID(actionID)
	if !ok {
		http.Error(w, "action no longer in catalog: "+actionID, http.StatusGone)
		return
	}
	args := map[string]string{}
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	user := currentUser(r)
	if user == "" {
		user = "ui:replay"
	}
	newID, err := s.runner.Enqueue(r.Context(), action, args, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", newID), http.StatusSeeOther)
}

// ──────────────────────────────────────────────────────────────────────
// WebSocket — live log stream for /tasks/<id>
// ──────────────────────────────────────────────────────────────────────

func (s *Server) handleTaskWS(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/ws/tasks/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	conn, err := s.upgr.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ch := s.runner.Subscribe(id)
	defer s.runner.Unsubscribe(id, ch)

	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})
	// Read goroutine — required for control frames; ignores text from client.
	go func() {
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	pingT := time.NewTicker(30 * time.Second)
	defer pingT.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				_ = conn.WriteJSON(LogEvent{TaskID: id, Status: "closed", At: nowMS()})
				return
			}
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		case <-pingT.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// Settings (dashboard / drill-down moved to /r/... in resourcehttp.go)
// ──────────────────────────────────────────────────────────────────────

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.renderSettings(w, r, map[string]any{
		"PullOK":       q.Get("pulled") == "1",
		"PullErr":      q.Get("err"),
		"TokenRevoked": q.Get("token_revoked") == "1",
	})
}

// renderSettings is the shared body for all settings-page render paths
// (GET /settings, POST /settings/api-tokens, password-update errors).
// Loads the bits the page always needs (commit, tokens) and merges in
// caller-provided extras like NewToken or TokenError.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	commit := strings.TrimSpace(captureCmd(s.cfg.RepoPath, "git", "rev-parse", "HEAD"))
	subject := strings.TrimSpace(captureCmd(s.cfg.RepoPath, "git", "log", "-1", "--pretty=%s"))
	tokens, _ := ListAPITokens(r.Context(), s.db)
	data := map[string]any{
		"Env":           s.cfg.Env,
		"User":          currentUser(r),
		"Commit":        commit,
		"CommitSubject": subject,
		"APITokens":     tokens,
	}
	for k, v := range extra {
		data[k] = v
	}
	s.render(w, "settings.html", data)
}

// handleSettingsPullRepo runs the same git fetch + reset that the
// background ticker does, but synchronously and on demand. Reloads
// actions.yml afterwards so the dashboard reflects any new actions
// without waiting for the next 30s tick.
func (s *Server) handleSettingsPullRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := pullRepoForRunner(s.cfg); err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if err := s.cat.Load(filepath.Join(s.cfg.RepoPath, "actions.yml")); err != nil {
		// Don't surface the catalog error as a hard failure — pull
		// itself succeeded; actions.yml might be transiently absent.
		http.Redirect(w, r, "/settings?pulled=1&err="+url.QueryEscape("repo pulled but actions reload: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?pulled=1", http.StatusSeeOther)
}

// pullRepoForRunner mirrors main.pullRepo. Duplicated here to avoid a
// circular import; both call out to git via os/exec.
func pullRepoForRunner(cfg *Config) error {
	out, err := captureExec(cfg.RepoPath, "git", "fetch", "--quiet", "origin", cfg.RepoBranch)
	if err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, out)
	}
	if out, err = captureExec(cfg.RepoPath, "git", "reset", "--hard", "origin/"+cfg.RepoBranch); err != nil {
		return fmt.Errorf("git reset: %w: %s", err, out)
	}
	return nil
}

func captureExec(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	b, err := c.CombinedOutput()
	return string(b), err
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	newPw := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	if newPw == "" || newPw != confirm {
		s.renderSettings(w, r, map[string]any{"Error": "passwords don't match (or empty)"})
		return
	}
	if err := SetAdminPassword(r.Context(), s.db, newPw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?ok=1", http.StatusSeeOther)
}

// ──────────────────────────────────────────────────────────────────────
// API token management (issued + revoked from /settings)
// ──────────────────────────────────────────────────────────────────────

// handleAPITokenCreate POSTs a `name`, mints a token, and re-renders
// the settings page with the freshly-minted token displayed once. We
// never persist the plaintext token (only its SHA-256), so the user
// must copy it now or re-issue. POST→render (not POST→redirect) so
// the token doesn't end up in URL/referrer logs.
func (s *Server) handleAPITokenCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	token, _, err := CreateAPIToken(r.Context(), s.db, name)
	if err != nil {
		s.renderSettings(w, r, map[string]any{"TokenError": err.Error()})
		return
	}
	s.renderSettings(w, r, map[string]any{"NewToken": token, "NewTokenName": name})
}

func (s *Server) handleAPITokenRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := RevokeAPIToken(r.Context(), s.db, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?token_revoked=1", http.StatusSeeOther)
}

// ──────────────────────────────────────────────────────────────────────
// Read-only JSON API
// ──────────────────────────────────────────────────────────────────────

const apiTokenNameKey ctxKey = 1

// requireAPIAuth wraps a handler with bearer-token authentication. On
// success the inner handler runs with hardened response headers
// (no-store / nosniff) and `currentAPITokenName(r)` exposes the token's
// human-readable name (so audit rows say `api:claude-debug` instead of
// just `api`). No CORS preflight handling — by design: the API isn't
// meant to be called from browsers.
func (s *Server) requireAPIAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const pfx = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, pfx) {
			apiErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(auth, pfx)
		_, name, err := VerifyAPIToken(r.Context(), s.db, token)
		if err != nil {
			apiErr(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx := context.WithValue(r.Context(), apiTokenNameKey, name)
		handler(w, r.WithContext(ctx))
	}
}

func currentAPITokenName(r *http.Request) string {
	if v, ok := r.Context().Value(apiTokenNameKey).(string); ok {
		return v
	}
	return "unknown"
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func apiJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// apiTaskRow is the JSON shape returned by /api/tasks endpoints. Mirrors
// the columns in `tasks` plus a derived has_log flag, with explicit
// json tags so any future column rename in the DB doesn't break clients.
type apiTaskRow struct {
	ID          int64           `json:"id"`
	ActionID    string          `json:"action_id"`
	ActionLabel string          `json:"action_label"`
	Status      string          `json:"status"`
	Args        json.RawMessage `json:"args"`
	Commit      string          `json:"commit,omitempty"`
	Username    string          `json:"username"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	EndedAt     *time.Time      `json:"ended_at,omitempty"`
	ExitCode    *int            `json:"exit_code,omitempty"`
	HasLog      bool            `json:"has_log"`
}

func scanAPITaskRow(scan func(...any) error) (apiTaskRow, string, error) {
	var (
		t       apiTaskRow
		argsStr string
		logPath string
	)
	err := scan(
		&t.ID, &t.ActionID, &t.ActionLabel, &t.Status, &argsStr,
		&t.Commit, &t.Username, &t.CreatedAt, &t.StartedAt, &t.EndedAt,
		&t.ExitCode, &logPath,
	)
	if err != nil {
		return t, "", err
	}
	t.Args = json.RawMessage(argsStr)
	t.HasLog = logPath != ""
	return t, logPath, nil
}

func (s *Server) handleAPITasksList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, action_id, action_label, status, args_json,
		        COALESCE(commit_hash,''), username, created_at,
		        started_at, ended_at, exit_code, COALESCE(log_path,'')
		 FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := make([]apiTaskRow, 0, limit)
	for rows.Next() {
		t, _, err := scanAPITaskRow(rows.Scan)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, t)
	}
	apiJSON(w, out)
}

// handleAPITask serves both /api/tasks/<id> (metadata JSON) and
// /api/tasks/<id>/log (the raw log file as text/plain). One handler
// because the path's prefix is shared and the variants are tiny.
func (s *Server) handleAPITask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "bad id")
		return
	}
	row := s.db.QueryRowContext(r.Context(),
		`SELECT id, action_id, action_label, status, args_json,
		        COALESCE(commit_hash,''), username, created_at,
		        started_at, ended_at, exit_code, COALESCE(log_path,'')
		 FROM tasks WHERE id=?`, id)
	t, logPath, err := scanAPITaskRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		apiErr(w, http.StatusNotFound, "no such task")
		return
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(parts) == 2 && parts[1] == "log" {
		if logPath == "" {
			apiErr(w, http.StatusNotFound, "no log yet")
			return
		}
		b, err := os.ReadFile(logPath)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// ?tail=N  → last N bytes (cheap "give me the failure tail").
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < len(b) {
				b = b[len(b)-n:]
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(b)
		return
	}
	apiJSON(w, t)
}

// handleAPIRunCreate enqueues a task. Same code path as the UI's run
// form (ValidateSubmission + Runner.Enqueue) so action-schema rules
// (required fields, regex patterns, enum sources) apply identically.
//
// Request:  POST /api/runs   { "action_id": "<id>", "args": {"k":"v",...} }
// Response: 201 { "id": <task_id>, "status": "queued", "url": "/tasks/<id>" }
//
// Username on the row is `api:<token-name>` so the audit trail shows
// which token initiated the run, not just "api".
func (s *Server) handleAPIRunCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		ActionID string            `json:"action_id"`
		Args     map[string]string `json:"args"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	action, ok := s.cat.ByID(body.ActionID)
	if !ok {
		apiErr(w, http.StatusNotFound, "no such action: "+body.ActionID)
		return
	}
	// Reuse ValidateSubmission's signature: it takes url.Values
	// (map[string][]string), which is what r.PostForm is. Wrap our
	// JSON map in that shape so the schema rules apply unchanged.
	form := make(map[string][]string, len(body.Args))
	for k, v := range body.Args {
		form[k] = []string{v}
	}
	args, err := ValidateSubmission(action, form)
	if err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	user := "api:" + currentAPITokenName(r)
	taskID, err := s.runner.Enqueue(r.Context(), action, args, user)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Headers MUST be set before WriteHeader; can't use apiJSON helper
	// here because it sets Content-Type internally and WriteHeader has
	// already locked the header set by then.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     taskID,
		"status": "queued",
		"url":    fmt.Sprintf("/tasks/%d", taskID),
	})
}

// ──────────────────────────────────────────────────────────────────────
// Render helpers
// ──────────────────────────────────────────────────────────────────────

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	// Auto-inject the chrome (env name + sidebar resource tree) for any
	// page rendered through the standard `_layout.html` header/footer.
	// Login renders a self-contained template and doesn't need the tree.
	if _, ok := data["Env"]; !ok {
		data["Env"] = s.cfg.Env
	}
	if _, ok := data["Tree"]; !ok && name != "login.html" {
		selected, _ := data["Selected"].(string)
		if tree, err := BuildResourceTree(s.cfg.Env, s.cfg.RepoPath, selected); err == nil {
			data["Tree"] = tree
		}
		// On error (e.g. inventory file missing during initial clone),
		// the layout template falls back to a minimal stub — no need to
		// 500 the page over a missing sidebar.
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func timeAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02 15:04")
	}
}
