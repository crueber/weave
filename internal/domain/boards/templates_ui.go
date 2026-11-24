package boards

import (
	"encoding/json"
	"fmt"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// The "New board" gallery: one sheet in the shared <dialog id="modal"> with a category rail, a search box, a grid of
// templates and a preview pane. Every pane is a server-rendered fragment swapped into #tg-main; the rail and the search
// box live in one form (#tg-form) whose values every request includes. The target project travels as ?projectId=.

// GalleryData feeds boards_gallery.html.
type GalleryData struct {
	ProjectID    string
	BoardID      string // "apply to this board" mode (board.template.apply): no blank/import tiles, preview ends in "Preview changes"
	BoardName    string
	Query        string
	Category     string // "" = all
	Categories   []TemplateCategory
	HasPublished bool
	Blank        bool           // show the "Blank board" tile first
	Builtins     []TemplateInfo // in category order
	Published    []TemplateInfo
	Empty        bool
}

// PreviewData feeds the preview pane.
type PreviewData struct {
	ProjectID string
	BoardID   string // apply mode
	BoardName string
	T         TemplatePreview
	Views     []string
	Cols      []PreviewCol
}

// PreviewCol is one column of the layout stub, with the sample cards that would land in it.
type PreviewCol struct {
	TemplateColumn
	Cards []PreviewCard
}

// PreviewCard is a sample card in the stub: label dots carry the resolved label colours.
type PreviewCard struct {
	Name   string
	Tasks  int
	Due    int // days from creation; 0 = none
	HasDue bool
	Dots   []string // label colours (as stored; the template resolves them)
}

func previewData(pid string, t TemplatePreview) PreviewData {
	d := PreviewData{ProjectID: pid, T: t, Views: viewList()}
	colour := map[string]string{}
	for _, l := range t.Labels {
		colour[l.Name] = l.Color
	}
	for _, c := range t.Columns {
		pc := PreviewCol{TemplateColumn: c}
		for _, k := range t.Cards {
			if k.Column != c.Name {
				continue
			}
			card := PreviewCard{Name: k.Name, Tasks: k.Tasks}
			if k.DueOffsetDays != nil {
				card.Due, card.HasDue = *k.DueOffsetDays, true
			}
			for _, l := range k.Labels {
				card.Dots = append(card.Dots, colour[l])
			}
			pc.Cards = append(pc.Cards, card)
		}
		d.Cols = append(d.Cols, pc)
	}
	return d
}

// galleryProject checks that the caller may create boards in the project named by ?projectId= and returns it.
func galleryProject(c *web.Ctx) (string, error) {
	pid := c.R.URL.Query().Get("projectId")
	if pid == "" {
		pid = c.R.PathValue("projectId")
	}
	if err := authz.RequireCreateBoard(c.Env, pid); err != nil {
		return "", err
	}
	return pid, nil
}

// galleryScope resolves the gallery's target: ?boardId= (apply a template to that board: board Manage, as the op) or
// the project (?projectId= or the path) for "New board".
func galleryScope(c *web.Ctx) (pid, boardID, boardName string, err error) {
	if bid := c.R.URL.Query().Get("boardId"); bid != "" {
		if err := authz.RequireBoard(c.Env, bid, authz.Manage); err != nil {
			return "", "", "", err
		}
		b, err := GetBoard(c.R.Context(), c.Env.DB, bid)
		if err != nil {
			return "", "", "", err
		}
		return b.ProjectID, b.ID, b.Name, nil
	}
	pid, err = galleryProject(c)
	return pid, "", "", err
}

func galleryData(c *web.Ctx, pid, q, cat string) (GalleryData, error) {
	d := GalleryData{ProjectID: pid, Query: q, Category: cat, Categories: TemplateCategories()}
	if cat != "" && cat != "published" && categoryName(cat) == "" {
		cat, d.Category = "", ""
	}
	all, err := ops.Typed[[]TemplateInfo](c.Env, "board.template.list", TemplateListIn{Query: q, Category: cat})
	if err != nil {
		return d, err
	}
	for _, t := range all {
		if t.ID == c.R.URL.Query().Get("boardId") && t.ID != "" {
			continue // a board is never applied to itself
		}
		if t.Source == "builtin" {
			d.Builtins = append(d.Builtins, t)
		} else {
			d.Published = append(d.Published, t)
		}
	}
	d.Blank = cat == "" && q == ""
	d.Empty = len(all) == 0 && !d.Blank
	if len(d.Published) > 0 {
		d.HasPublished = true
	} else if pub, err := ops.Typed[[]TemplateInfo](c.Env, "board.template.list", TemplateListIn{Category: "published"}); err == nil {
		d.HasPublished = len(pub) > 0
	}
	return d, nil
}

func registerGalleryUI() {
	// the whole sheet
	web.Route("GET /ui/projects/{projectId}/boards/new", func(c *web.Ctx) error {
		pid, err := galleryProject(c)
		if err != nil {
			return err
		}
		d, err := galleryData(c, pid, "", "")
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_gallery.html", "boards_gallery", d)
	})
	// the same sheet in "apply to this board" mode (Board settings > Templates & export)
	web.Route("GET /ui/boards/{id}/apply-template", func(c *web.Ctx) error {
		q := c.R.URL.Query()
		q.Set("boardId", c.R.PathValue("id"))
		c.R.URL.RawQuery = q.Encode()
		pid, bid, bname, err := galleryScope(c)
		if err != nil {
			return err
		}
		d, err := galleryData(c, pid, "", "")
		if err != nil {
			return err
		}
		d.BoardID, d.BoardName, d.Blank = bid, bname, false
		d.Empty = len(d.Builtins)+len(d.Published) == 0
		return c.RenderBlock("boards_gallery.html", "boards_gallery", d)
	})
	// results grid (op board.template.list): search words and category come from the rail/search form
	web.Route(ops.Get("board.template.list").UI, func(c *web.Ctx) error {
		pid, bid, bname, err := galleryScope(c)
		if err != nil {
			return err
		}
		d, err := galleryData(c, pid, c.R.URL.Query().Get("q"), c.R.URL.Query().Get("category"))
		if err != nil {
			return err
		}
		if bid != "" {
			d.BoardID, d.BoardName, d.Blank = bid, bname, false
			d.Empty = len(d.Builtins)+len(d.Published) == 0
		}
		return c.RenderBlock("boards_gallery.html", "boards_tpl_grid", d)
	})
	// preview of one template (op board.template.get)
	web.Route(ops.Get("board.template.get").UI, func(c *web.Ctx) error {
		pid, bid, bname, err := galleryScope(c)
		if err != nil {
			return err
		}
		t, err := ops.Typed[TemplatePreview](c.Env, "board.template.get", TemplateGetIn{TemplateID: c.R.PathValue("templateId")})
		if err != nil {
			return err
		}
		pd := previewData(pid, t)
		pd.BoardID, pd.BoardName = bid, bname
		return c.RenderBlock("boards_gallery.html", "boards_tpl_preview", pd)
	})
	// board.template.apply: the preview (dryRun) renders the list of changes into the sheet; the real apply goes to
	// the board with a toast
	web.Route(ops.Get("board.template.apply").UI, func(c *web.Ctx) error {
		in, err := c.Bind("board.template.apply")
		if err != nil {
			return err
		}
		ai := in.(*TemplateApplyIn)
		out, err := ops.Typed[TemplateApplyOut](c.Env, "board.template.apply", ai)
		if err != nil {
			return err
		}
		if ai.DryRun {
			b, err := GetBoard(c.R.Context(), c.Env.DB, ai.ID)
			if err != nil {
				return err
			}
			t, err := ops.Typed[TemplatePreview](c.Env, "board.template.get", TemplateGetIn{TemplateID: ai.TemplateID})
			if err != nil {
				return err
			}
			return c.RenderBlock("boards_gallery.html", "boards_tpl_apply", ApplyPreviewData{BoardID: ai.ID, BoardName: b.Name, In: *ai, Out: out, T: t})
		}
		msg := fmt.Sprintf("Applied %s: added %s, moved %s into %s", out.TemplateName, plural(len(out.ColumnsAdded), "column"), plural(out.CardsMoved, "card"), out.TargetColumn.Name)
		if n := len(out.ColumnsRemoved); n > 0 {
			msg += ", removed " + plural(n, "old column")
		}
		c.W.Header().Set("HX-Trigger", toastNext(msg))
		c.Redirect("/boards/" + ai.ID)
		return nil
	})
	web.Route("GET /ui/projects/{projectId}/boards/new/blank", func(c *web.Ctx) error {
		pid, err := galleryProject(c)
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_gallery.html", "boards_tpl_blank", PreviewData{ProjectID: pid, Views: viewList()})
	})
	web.Route("GET /ui/projects/{projectId}/boards/new/import", func(c *web.Ctx) error {
		pid, err := galleryProject(c)
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_gallery.html", "boards_tpl_import", PreviewData{ProjectID: pid})
	})
}

// ApplyPreviewData feeds the "apply to this board" preview (boards_tpl_apply).
type ApplyPreviewData struct {
	BoardID   string
	BoardName string
	In        TemplateApplyIn
	Out       TemplateApplyOut
	T         TemplatePreview
}

// toastNext is an HX-Trigger header value for a toast shown on the next page (weave.js keeps it across the redirect).
func toastNext(msg string) string {
	b, _ := json.Marshal(map[string]string{"toastNext": msg})
	return string(b)
}

func viewList() []string { return []string{"kanban", "list", "grid", "calendar", "map"} }
