package internal

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// TaskScope is exposed via template func "taskScope" (see NewServer).
// Given an action id + the masked args_json from the DB, returns the
// human-friendly scope string (the same one shown as a chip in the
// bottom console). Returns "" when the action no longer exists.
func (c *Catalog) TaskScope(actionID, argsJSON string) string {
	if actionID == "" {
		return ""
	}
	a, ok := c.ByID(actionID)
	if !ok {
		return ""
	}
	var args map[string]string
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}
	return scopeFromArgs(a, args)
}

// taskDuration returns a short "took X" / "running X" / "—" string for
// the tasks listing's "Took" column. Captures three states:
//
//   - terminal (success/error/cancelled): ended_at - started_at
//   - running:                            now - started_at
//   - queued (no started_at yet):         "—"
//
// Takes pointers explicitly so it works for any struct that has
// nullable started/ended timestamps (taskRow, taskDetail, …) without
// the template needing to know the row's underlying type.
func taskDuration(started, ended *time.Time) string {
	if started == nil {
		return "—"
	}
	end := time.Now()
	if ended != nil {
		end = *ended
	}
	d := end.Sub(*started)
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// derefTime dereferences a *time.Time so templates can call timeAgo on
// nullable fields without exploding on nil. Returns the zero time, which
// timeAgo formats as "—".
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// pageURLBase builds the query-string prefix used by the tasks-page
// pagination links. It preserves all filters except `page`, so clicking
// "next" doesn't reset the current status / action / search filters.
// The returned string ends with "?" or "&" — the caller appends
// `page=N`.
func pageURLBase(f taskFilters) string {
	v := url.Values{}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if f.ActionID != "" {
		v.Set("action", f.ActionID)
	}
	if f.Search != "" {
		v.Set("q", f.Search)
	}
	if f.PageSize != 0 && f.PageSize != 25 {
		v.Set("page_size", fmt.Sprintf("%d", f.PageSize))
	}
	if len(v) == 0 {
		return "?"
	}
	return "?" + v.Encode() + "&"
}
