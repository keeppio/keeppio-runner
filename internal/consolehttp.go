package internal

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// consoleTask is the JSON shape served to the bottom console for the
// recent-tasks tab strip. Times are unix millis (consistent with the
// WebSocket LogEvent.At field) — keeps client-side rendering simple.
type consoleTask struct {
	ID          int64             `json:"id"`
	ActionID    string            `json:"action_id"`
	ActionLabel string            `json:"action_label"`
	Status      string            `json:"status"`
	Username    string            `json:"username"`
	CreatedAt   int64             `json:"created_at"`
	StartedAt   *int64            `json:"started_at,omitempty"`
	EndedAt     *int64            `json:"ended_at,omitempty"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	Scope       string            `json:"scope"`
	Args        map[string]string `json:"args,omitempty"`
	OutOfScope  bool              `json:"out_of_scope,omitempty"`
}

// handleConsoleRecent serves the recent-tasks tab strip data for the
// bottom console.
//
//   - `?limit=N`    cap returned rows (default 20, max 100)
//   - `?scope=PATH` apply scope filter (e.g. "api-01/demo"). When set,
//                   completed tasks must match the last path segment via
//                   scopeFromArgs; running/queued tasks always pass the
//                   filter but are flagged `out_of_scope: true` if they
//                   don't match — keeps in-flight work visible.
//   - `?show_all=1` ignore scope filtering (operator override).
func (s *Server) handleConsoleRecent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 20
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 {
		if l > 100 {
			l = 100
		}
		limit = l
	}
	scope := strings.TrimSpace(q.Get("scope"))
	showAll := q.Get("show_all") == "1"
	scopeToken := ""
	if !showAll && scope != "" {
		// Last path segment is the primary resource name (server slug
		// or tenant slug). Matches scopeFromArgs which returns the
		// first applies_to.field value — typically the same.
		scopeToken = lastPathSegment(scope)
	}

	// Over-fetch when filtering so we still return ~limit matches.
	fetch := limit
	if scopeToken != "" {
		fetch = limit * 5
		if fetch > 500 {
			fetch = 500
		}
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, action_id, action_label, status, COALESCE(args_json,'{}'),
		        COALESCE(username,''), created_at, started_at, ended_at, exit_code
		 FROM tasks ORDER BY id DESC LIMIT ?`, fetch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []consoleTask
	for rows.Next() {
		var t consoleTask
		var argsJSON string
		var createdAt time.Time
		var startedAt, endedAt sql.NullTime
		var exitCode sql.NullInt64
		if err := rows.Scan(&t.ID, &t.ActionID, &t.ActionLabel, &t.Status, &argsJSON,
			&t.Username, &createdAt, &startedAt, &endedAt, &exitCode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		t.CreatedAt = createdAt.UnixMilli()
		if startedAt.Valid {
			v := startedAt.Time.UnixMilli()
			t.StartedAt = &v
		}
		if endedAt.Valid {
			v := endedAt.Time.UnixMilli()
			t.EndedAt = &v
		}
		if exitCode.Valid {
			v := int(exitCode.Int64)
			t.ExitCode = &v
		}
		// args_json is the masked DB version — safe to expose.
		_ = json.Unmarshal([]byte(argsJSON), &t.Args)
		if action, ok := s.cat.ByID(t.ActionID); ok {
			t.Scope = scopeFromArgs(action, t.Args)
		}
		// Scope filter (after computing scope so OutOfScope is accurate).
		if scopeToken != "" {
			matches := t.Scope == scopeToken
			if !matches {
				// Also match by direct arg value — covers actions whose
				// scopeFromArgs returns a different field than the one
				// the operator drilled into.
				for _, v := range t.Args {
					if v == scopeToken {
						matches = true
						break
					}
				}
			}
			if !matches {
				inFlight := t.Status == "running" || t.Status == "queued"
				if !inFlight {
					continue
				}
				t.OutOfScope = true
			}
		}
		out = append(out, t)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tasks":  out,
		"scope":  scope,
		"filter": scopeToken,
	})
}

// handleUITaskLog streams a task's log file as plain text. Cookie auth
// (so the bottom console fetches it without a bearer token). Used to
// display the log of a finished task selected from the console tab
// strip — running tasks open a WebSocket instead.
func (s *Server) handleUITaskLog(w http.ResponseWriter, r *http.Request) {
	// /ui/tasks/<id>/log
	if !strings.HasSuffix(r.URL.Path, "/log") {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/ui/tasks/")
	rest = strings.TrimSuffix(rest, "/log")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var logPath sql.NullString
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT log_path FROM tasks WHERE id = ?`, id).Scan(&logPath); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !logPath.Valid || logPath.String == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("(no log file yet)"))
		return
	}
	b, err := os.ReadFile(logPath.String)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("(log unreadable: " + err.Error() + ")"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// handleCatalogJSON returns a slim catalog snapshot for the command
// palette: id, label, description, group, severity, plus the inventory
// groups the action applies to. Used client-side to build a fuzzy
// search over actions without re-implementing the YAML loader in JS.
func (s *Server) handleCatalogJSON(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID          string   `json:"id"`
		Label       string   `json:"label"`
		Description string   `json:"description,omitempty"`
		Group       string   `json:"group,omitempty"`
		Severity    string   `json:"severity,omitempty"`
		AppliesTo   []string `json:"applies_to,omitempty"`
	}
	out := make([]item, 0, len(s.cat.All()))
	for _, a := range s.cat.All() {
		groups := make([]string, 0, len(a.AppliesTo))
		for _, ap := range a.AppliesTo {
			groups = append(groups, ap.Group)
		}
		out = append(out, item{
			ID: a.ID, Label: a.Label, Description: a.Description,
			Group: a.Group, Severity: a.Severity, AppliesTo: groups,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{"actions": out})
}

func lastPathSegment(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
