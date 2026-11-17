package boards

import (
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
	if err := authz.RequireProject(c.Env, pid, authz.Manage); err != nil {
		return "", err
	}
	return pid, nil
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
	// results grid (op board.template.list): search words and category come from the rail/search form
	web.Route(ops.Get("board.template.list").UI, func(c *web.Ctx) error {
		pid, err := galleryProject(c)
		if err != nil {
			return err
		}
		d, err := galleryData(c, pid, c.R.URL.Query().Get("q"), c.R.URL.Query().Get("category"))
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_gallery.html", "boards_tpl_grid", d)
	})
	// preview of one template (op board.template.get)
	web.Route(ops.Get("board.template.get").UI, func(c *web.Ctx) error {
		pid, err := galleryProject(c)
		if err != nil {
			return err
		}
		t, err := ops.Typed[TemplatePreview](c.Env, "board.template.get", TemplateGetIn{TemplateID: c.R.PathValue("templateId")})
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_gallery.html", "boards_tpl_preview", previewData(pid, t))
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

func viewList() []string { return []string{"kanban", "list", "grid", "calendar", "map"} }
