package internal

import (
	"sort"
	"strings"
)

// ToolbarAction is the shape the resource view renders for each
// applicable action button. URLs are pre-built to match the existing
// `/run/<id>?<field>=<value>` convention so the action form arrives
// pre-filled. The modal-mode link variant is added in phase 5; for
// now the link is a normal full-page navigation.
type ToolbarAction struct {
	ID            string
	Label         string
	Description   string
	Group         string
	Severity      string // "danger" | other
	IsDestructive bool   // severity=="danger" (drives confirm UI in phase 5)
	Href          string // /run/<id>?field=value (or /run/<id> if no prefill)
	ModalHref     string // /run/<id>?modal=1&field=value (HTMX target in phase 5)
}

// Toolbar groups applicable actions for one resource into:
//
//   - Inline:      shown as toolbar buttons at the top of the view.
//   - Maintenance: hidden behind a "Maintenance ▾" dropdown.
//
// The split is policy, not data — keeps dangerous / rarely-used actions
// one extra click away. Group names matching maintenanceGroupPatterns
// (case-insensitive substring) and any severity=danger action land in
// Maintenance regardless of group.
type Toolbar struct {
	Inline      []ToolbarAction
	Maintenance []ToolbarAction
}

var maintenanceGroupPatterns = []string{
	"maintenance",
	"destructive",
	"danger",
	"cleanup",
	"restore",
	"backup",
	"rebuild",
}

func isMaintenanceGroup(group string) bool {
	g := strings.ToLower(group)
	for _, p := range maintenanceGroupPatterns {
		if strings.Contains(g, p) {
			return true
		}
	}
	return false
}

// BuildToolbar returns the toolbar split for a single resource.
//
// `resourceGroup` is the inventory group the resource belongs to
// (clients|servers|ops|...). `resourceName` is the specific host slug
// — used to pre-fill the form field declared in `applies_to`.
func (c *Catalog) BuildToolbar(resourceGroup, resourceName string) Toolbar {
	var t Toolbar
	for _, a := range c.All() {
		var prefillField string
		var matched bool
		for _, ap := range a.AppliesTo {
			if ap.Group == resourceGroup {
				prefillField = ap.Field
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		ta := ToolbarAction{
			ID:            a.ID,
			Label:         a.Label,
			Description:   a.Description,
			Group:         a.Group,
			Severity:      a.Severity,
			IsDestructive: a.Severity == "danger",
		}
		ta.Href, ta.ModalHref = runHrefs(a.ID, prefillField, resourceName)
		if isMaintenanceGroup(a.Group) || ta.IsDestructive {
			t.Maintenance = append(t.Maintenance, ta)
		} else {
			t.Inline = append(t.Inline, ta)
		}
	}
	sort.Slice(t.Inline, func(i, j int) bool { return t.Inline[i].Label < t.Inline[j].Label })
	sort.Slice(t.Maintenance, func(i, j int) bool {
		// Maintenance groups sort first by group name, then label —
		// keeps "Backup → Restore" sequences readable in the dropdown.
		if t.Maintenance[i].Group != t.Maintenance[j].Group {
			return t.Maintenance[i].Group < t.Maintenance[j].Group
		}
		return t.Maintenance[i].Label < t.Maintenance[j].Label
	})
	return t
}

func runHrefs(actionID, field, value string) (string, string) {
	base := "/run/" + actionID
	modal := base + "?modal=1"
	if field != "" && value != "" {
		q := "?" + field + "=" + value
		base += q
		modal += "&" + field + "=" + value
	}
	return base, modal
}
