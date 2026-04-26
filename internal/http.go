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
	"os"
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
	cfg    *Config
	db     *sql.DB
	cat    *Catalog
	runner *Runner
	tmpl   *template.Template
	upgr   websocket.Upgrader
}

func NewServer(cfg *Config, db *sql.DB, cat *Catalog, runner *Runner, tplFS embed.FS) (*Server, error) {
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
	}).ParseFS(tplFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		db:     db,
		cat:    cat,
		runner: runner,
		tmpl:   tmpl,
		upgr: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}, nil
}

func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/login", s.handleLoginGet)
	mux.HandleFunc("/login.submit", s.handleLoginPost)
	mux.HandleFunc("/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("/", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/run/", s.requireAuth(s.handleRun))                  // GET: form, POST: enqueue
	mux.HandleFunc("/tasks", s.requireAuth(s.handleTasksList))
	mux.HandleFunc("/tasks/", s.requireAuth(s.handleTaskShow))           // /tasks/<id>
	mux.HandleFunc("/tasks/cancel/", s.requireAuth(s.handleTaskCancel))  // POST
	mux.HandleFunc("/inventory", s.requireAuth(s.handleInventory))
	mux.HandleFunc("/settings", s.requireAuth(s.handleSettingsGet))
	mux.HandleFunc("/settings.submit", s.requireAuth(s.handleSettingsPost))
	mux.HandleFunc("/ws/tasks/", s.requireAuth(s.handleTaskWS))          // /ws/tasks/<id>
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
	username := r.FormValue("username")
	password := r.FormValue("password")
	if username != "admin" {
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
	id, err := NewSession(r.Context(), s.db, "admin")
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
// Dashboard + action form
// ──────────────────────────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, "dashboard.html", map[string]any{
		"Env":    s.cfg.Env,
		"User":   currentUser(r),
		"Groups": s.cat.Grouped(),
	})
}

// /run/<id>  GET = form, POST = enqueue (POSTed to /run/<id>.submit
// to keep the GET clean for permalink-friendly URLs).
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/run/")
	id = strings.TrimSuffix(id, ".submit")
	action, ok := s.cat.ByID(id)
	if !ok {
		http.Error(w, "no such action", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPost {
		s.handleRunSubmit(w, r, action)
		return
	}
	// Resolve dropdowns at form-render time. Failures show inline so
	// the operator can see WHY a list is empty.
	fields := make([]resolvedField, 0, len(action.Fields))
	for _, f := range action.Fields {
		rf := resolvedField{Field: f}
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
	s.render(w, "run.html", map[string]any{
		"Env":    s.cfg.Env,
		"User":   currentUser(r),
		"Action": action,
		"Fields": fields,
	})
}

func (s *Server) handleRunSubmit(w http.ResponseWriter, r *http.Request, action Action) {
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
		s.render(w, "run.html", map[string]any{
			"Env":    s.cfg.Env,
			"User":   currentUser(r),
			"Action": action,
			"Fields": fields,
			"Error":  err.Error(),
		})
		return
	}
	taskID, err := s.runner.Enqueue(r.Context(), action, args, currentUser(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", taskID), http.StatusSeeOther)
}

// ──────────────────────────────────────────────────────────────────────
// Task list + detail + cancel
// ──────────────────────────────────────────────────────────────────────

type taskRow struct {
	ID         int64
	ActionLabel string
	Status     string
	ArgsJSON   string
	CommitHash string
	Username   string
	CreatedAt  time.Time
	StartedAt  *time.Time
	EndedAt    *time.Time
	ExitCode   *int
}

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, action_label, status, args_json, COALESCE(commit_hash,''), username, created_at, started_at, ended_at, exit_code
		 FROM tasks ORDER BY id DESC LIMIT 100`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var tasks []taskRow
	for rows.Next() {
		var t taskRow
		if err := rows.Scan(&t.ID, &t.ActionLabel, &t.Status, &t.ArgsJSON, &t.CommitHash, &t.Username, &t.CreatedAt, &t.StartedAt, &t.EndedAt, &t.ExitCode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tasks = append(tasks, t)
	}
	s.render(w, "tasks.html", map[string]any{
		"Env":   s.cfg.Env,
		"User":  currentUser(r),
		"Tasks": tasks,
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
// Inventory browser + settings
// ──────────────────────────────────────────────────────────────────────

func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	hostsPath := filepath.Join(s.cfg.RepoPath, "inventories", s.cfg.Env, "hosts.yml")
	hostsBody, _ := os.ReadFile(hostsPath)
	s.render(w, "inventory.html", map[string]any{
		"Env":       s.cfg.Env,
		"User":      currentUser(r),
		"HostsBody": string(hostsBody),
	})
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	commit := strings.TrimSpace(captureCmd(s.cfg.RepoPath, "git", "rev-parse", "HEAD"))
	s.render(w, "settings.html", map[string]any{
		"Env":    s.cfg.Env,
		"User":   currentUser(r),
		"Commit": commit,
	})
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	newPw := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	if newPw == "" || newPw != confirm {
		commit := strings.TrimSpace(captureCmd(s.cfg.RepoPath, "git", "rev-parse", "HEAD"))
		s.render(w, "settings.html", map[string]any{
			"Env":    s.cfg.Env,
			"User":   currentUser(r),
			"Commit": commit,
			"Error":  "passwords don't match (or empty)",
		})
		return
	}
	if err := SetAdminPassword(r.Context(), s.db, newPw); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?ok=1", http.StatusSeeOther)
}

// ──────────────────────────────────────────────────────────────────────
// Render helpers
// ──────────────────────────────────────────────────────────────────────

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
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
