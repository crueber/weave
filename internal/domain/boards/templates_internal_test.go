package boards

import (
	"strings"
	"testing"
)

// Every embedded template must parse and obey the catalogue rules (no absolute dates, no people, preset colours only...).
func TestBuiltinTemplatesAreValid(t *testing.T) {
	list := loadBuiltins()
	if len(builtinErrs) > 0 {
		t.Fatalf("manifests failed to parse: %v", builtinErrs)
	}
	if len(list) != 14 {
		t.Fatalf("want 14 built-in templates, have %d", len(list))
	}
	preset := map[string]bool{}
	for _, p := range Presets {
		preset[p.Name] = true
	}
	ids, names := map[string]bool{}, map[string]bool{}
	core := map[string]bool{"builtin:kanban-basics": true, "builtin:sprint": true, "builtin:bug-tracker": true, "builtin:product-roadmap": true}
	for _, b := range list {
		m, ib := b.m, b.ib
		t.Run(m.ID, func(t *testing.T) {
			if ids[m.ID] || names[m.Name] {
				t.Fatalf("duplicate id or name")
			}
			ids[m.ID], names[m.Name] = true, true
			if m.Summary == "" || m.Audience == "" || len(m.Tags) == 0 || len(m.Summary) > 140 {
				t.Errorf("summary (<=140), audience and tags are required")
			}
			if len(m.Name) > 40 || len(m.Board.Lists) < 3 || len(m.Board.Lists) > 8 || len(ib.Cards) > 4 || len(ib.Labels) > 8 {
				t.Errorf("size caps: name %d lists %d cards %d labels %d", len(m.Name), len(m.Board.Lists), len(ib.Cards), len(ib.Labels))
			}
			if m.SampleCardsDefault != core[m.ID] || (m.SampleCardsDefault && !m.IncludesSampleCards) {
				t.Errorf("sampleCardsDefault must be on exactly for the four core templates")
			}
			if len(ib.Cards) > 0 != m.IncludesSampleCards {
				t.Errorf("includesSampleCards out of sync")
			}
			if m.Board.Board["color"] != nil && !preset[m.Board.Board["color"].(string)] {
				t.Errorf("board colour %v is not a preset name", m.Board.Board["color"])
			}
			lists := map[string]bool{}
			for _, l := range m.Board.Lists {
				if l.Type != "active" && l.Type != "closed" {
					t.Errorf("list %q: only active and closed lists", l.Name)
				}
				if l.Color != "" && !preset[l.Color] {
					t.Errorf("list %q colour %q is not a preset name", l.Name, l.Color)
				}
				if l.WIPLimit != 0 && (l.WIPLimit < 1 || l.WIPLimit > MaxWIPLimit) {
					t.Errorf("list %q WIP %d out of range", l.Name, l.WIPLimit)
				}
				if l.ArchiveAfter != 0 && (l.ArchiveAfter < 1 || l.ArchiveAfter > MaxArchiveAfter) {
					t.Errorf("list %q archiveAfter %d out of range", l.Name, l.ArchiveAfter)
				}
				if l.Name == "" || lists[l.ID] || len(l.Description) > 400 {
					t.Errorf("list %q: bad name/id/description", l.Name)
				}
				lists[l.ID] = true
			}
			if len(ib.Lists) != len(m.Board.Lists) {
				t.Errorf("a list was lost while parsing")
			}
			labels := map[string]bool{}
			for _, l := range m.Board.Labels {
				if !preset[l.Color] {
					t.Errorf("label %q colour %q is not a preset name", l.Name, l.Color)
				}
				labels[l.ID] = true
			}
			groupIDs := map[string]bool{}
			for _, g := range m.Board.Groups {
				groupIDs[g.ID] = true
			}
			for _, l := range m.Board.Labels {
				if l.GroupID != "" && !groupIDs[l.GroupID] {
					t.Errorf("label %q references a missing group", l.Name)
				}
			}
			cardIDs := map[string]bool{}
			for _, c := range m.Board.Cards {
				if !lists[c.ListID] {
					t.Errorf("card %q references a missing list", c.Name)
				}
				if c.DueDate != "" {
					t.Errorf("card %q: absolute due date; use dueOffsetDays", c.Name)
				}
				if c.DueOffsetDays != nil && (*c.DueOffsetDays < 0 || *c.DueOffsetDays > 90) {
					t.Errorf("card %q: offset %d not sane", c.Name, *c.DueOffsetDays)
				}
				if len(c.Comments) > 0 || len(c.MemberEmails) > 0 || c.IsDueCompleted {
					t.Errorf("card %q: comments, members and completion are not allowed", c.Name)
				}
				for _, l := range c.LabelIDs {
					if !labels[l] {
						t.Errorf("card %q references a missing label", c.Name)
					}
				}
				for _, tl := range c.TaskLists {
					for _, tk := range tl.Tasks {
						if tk.IsCompleted {
							t.Errorf("card %q: tasks start unchecked", c.Name)
						}
					}
				}
				if c.ID == "" || cardIDs[c.ID] || c.Name == "" {
					t.Errorf("card %q: bad id/name", c.Name)
				}
				cardIDs[c.ID] = true
			}
			fields := map[string]bool{}
			for _, g := range m.Board.CustomFields {
				for _, f := range g.Fields {
					fields[g.ID+"/"+f.ID] = true
				}
			}
			for _, c := range m.Board.Cards {
				for _, v := range c.CustomValues {
					if !fields[v.GroupID+"/"+v.FieldID] {
						t.Errorf("card %q: value for an unknown field", c.Name)
					}
				}
			}
			if !strings.HasPrefix(m.ID, "builtin:") || categoryName(m.Category) == "" {
				t.Errorf("bad id or category")
			}
		})
	}
}
