package cards

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

const archiveUIPageSize = 50

// archiveUIData feeds both the full Archive fragment and its cursor-page response. The visible q,
// label and member controls stay in the board filter row; their current values are mirrored as hidden
// fields here so the archive-only facets can be submitted on their own.
type archiveUIData struct {
	Board      boards.Board
	Kind       string
	View       string
	Caps       authz.BoardCaps
	Out        ArchiveListOut
	Lists      []boards.List
	Rows       []archiveUIRow
	Q          string
	LabelID    string
	MemberID   string
	FromListID string
	Since      string
	Until      string
	Sort       string
	Dir        string
	Filtered   bool
	RestoreURL string
	DeleteURL  string
	NextURL    string
}

type archiveUIRow struct {
	Item     ArchiveListItem
	Month    string
	NewMonth bool
}

func init() {
	web.AddFuncs(template.FuncMap{
		"archiveAgo":       archiveAgo,
		"archiveDate":      archiveDate,
		"archiveDateInput": archiveDateInput,
		"archiveCardIDs":   archiveCardIDs,
	})

	web.Route("GET /ui/boards/{boardId}/archive", func(c *web.Ctx) error {
		return renderArchive(c, c.R.URL.Query().Get("cursor") != "")
	})
	web.Route("POST /ui/boards/{boardId}/archive/restore", func(c *web.Ctx) error {
		return renderArchiveBulk(c, "card.restore.bulk")
	})
	web.Route("POST /ui/boards/{boardId}/archive/delete", func(c *web.Ctx) error {
		return renderArchiveBulk(c, "card.delete.bulk")
	})
}

func archiveAgo(raw string) string {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("Jan 2, 2006")
	}
}

func archiveDate(raw string) string {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	return t.Local().Format("Jan 2, 2006 15:04 MST")
}

func archiveDateInput(raw string) string {
	if len(raw) >= 10 {
		return raw[:10]
	}
	return raw
}

func archiveCardIDs(id string) string {
	b, _ := json.Marshal([]string{id})
	return string(b)
}

func archiveKindOf(q url.Values) string {
	for _, key := range []string{"show", "kind"} {
		switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
		case "trash":
			return "trash"
		case "archived":
			return "archived"
		}
	}
	return "archived"
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// normalizeArchiveUIQuery maps the board filter row's repeated label/member fields onto the archive
// op's singular filters, maps show to kind, and fixes the browser page size at 50.
func normalizeArchiveUIQuery(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for key, values := range q {
		out[key] = append([]string(nil), values...)
	}
	kind := archiveKindOf(out)
	out.Set("show", kind)
	out.Set("kind", kind)
	out.Set("limit", fmt.Sprint(archiveUIPageSize))
	if firstNonEmpty(out["labelId"]) == "" {
		out.Set("labelId", firstNonEmpty(out["label"]))
	}
	if firstNonEmpty(out["memberId"]) == "" {
		out.Set("memberId", firstNonEmpty(out["member"]))
	}
	return out
}

func archiveView(q url.Values, fallback string) string {
	switch q.Get("view") {
	case "list", "grid", "kanban":
		return q.Get("view")
	default:
		if fallback == "list" || fallback == "grid" {
			return fallback
		}
		return "kanban"
	}
}

func archiveURL(path string, q url.Values) string {
	out := url.Values{}
	for _, key := range []string{"q", "label", "member", "labelId", "memberId", "fromListId", "since", "until", "sort", "dir", "view", "show", "kind", "cursor"} {
		for _, value := range q[key] {
			if value != "" {
				out[key] = append(out[key], value)
			}
		}
	}
	return path + "?" + out.Encode()
}

func archiveMonthOf(raw string) (key, label string) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw, raw
	}
	t = t.Local()
	return t.Format("2006-01"), t.Format("January 2006")
}

func archiveRows(items []ArchiveListItem, sort, previousRaw string) []archiveUIRow {
	rows := make([]archiveUIRow, 0, len(items))
	if sort != "archived" {
		for _, item := range items {
			rows = append(rows, archiveUIRow{Item: item})
		}
		return rows
	}
	previous, _ := archiveMonthOf(previousRaw)
	for _, item := range items {
		key, label := archiveMonthOf(item.ArchivedAt)
		rows = append(rows, archiveUIRow{Item: item, Month: label, NewMonth: key != previous})
		previous = key
	}
	return rows
}

func renderArchive(c *web.Ctx, appendRows bool) error {
	boardID := c.R.PathValue("boardId")
	q := normalizeArchiveUIQuery(c.R.URL.Query())
	c.R.URL.RawQuery = q.Encode()

	outAny, err := c.Do("card.archive.list")
	if err != nil {
		return err
	}
	out, ok := outAny.(ArchiveListOut)
	if !ok {
		return fmt.Errorf("card.archive.list returned %T", outAny)
	}
	detail, err := ops.Typed[boards.BoardDetail](c.Env, "board.get", boards.BoardGetIn{ID: boardID})
	if err != nil {
		return err
	}
	kind := archiveKindOf(q)
	view := archiveView(q, detail.Board.DefaultView)
	q.Set("view", view)
	previousRaw := ""
	if cursor := q.Get("cursor"); cursor != "" {
		if cursorData, err := decodeArchiveCursor(c.Env, cursor); err == nil {
			previousRaw = cursorData.ArchivedAt
		}
	}
	d := archiveUIData{
		Board: detail.Board, Kind: kind, View: view, Caps: authz.Board(c.Env, boardID), Out: out,
		Q: q.Get("q"), LabelID: firstNonEmpty(q["labelId"]), MemberID: firstNonEmpty(q["memberId"]),
		FromListID: q.Get("fromListId"), Since: q.Get("since"), Until: q.Get("until"),
		Sort: q.Get("sort"), Dir: q.Get("dir"),
	}
	if d.Sort == "" {
		d.Sort = "archived"
	}
	if d.Dir == "" {
		d.Dir = "desc"
	}
	d.Filtered = d.Q != "" || d.LabelID != "" || d.MemberID != "" || d.FromListID != "" || d.Since != "" || d.Until != ""
	for _, list := range detail.Lists {
		if list.Type == "active" || list.Type == "closed" {
			d.Lists = append(d.Lists, list)
		}
	}
	d.Rows = archiveRows(out.Items, d.Sort, previousRaw)
	base := "/ui/boards/" + boardID + "/archive"
	actionQuery := normalizeArchiveUIQuery(q)
	actionQuery.Del("cursor")
	d.RestoreURL = archiveURL(base+"/restore", actionQuery)
	d.DeleteURL = archiveURL(base+"/delete", actionQuery)
	if out.NextCursor != "" {
		next := normalizeArchiveUIQuery(q)
		next.Set("cursor", out.NextCursor)
		d.NextURL = archiveURL(base, next)
	}
	if appendRows {
		return c.RenderBlock("cards_archive.html", "cards_archive_page", d)
	}
	return c.RenderBlock("cards_archive.html", "cards_archive", d)
}

// prepareArchiveBulkForm keeps the HTML form contract convenient: the bulk form always carries the
// current filter fields, but explicit card selections must not send them (the op allows filters only
// with all=true). JSON clients keep their body untouched.
func prepareArchiveBulkForm(c *web.Ctx) error {
	if strings.HasPrefix(c.R.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	c.R.Body = http.MaxBytesReader(c.W, c.R.Body, ops.DefaultMaxBody)
	if err := c.R.ParseForm(); err != nil {
		return ops.Invalid("invalid form data")
	}
	if c.R.Form.Get("kind") == "" {
		kind := archiveKindOf(c.R.URL.Query())
		c.R.Form.Set("kind", kind)
	}
	all := c.R.Form.Get("all") == "true" || c.R.Form.Get("all") == "1" || c.R.Form.Get("all") == "on"
	if !all {
		for _, key := range []string{"q", "labelId", "memberId", "fromListId", "since", "until", "confirmCount"} {
			c.R.Form.Del(key)
		}
	}
	return nil
}

func renderArchiveBulk(c *web.Ctx, op string) error {
	if err := prepareArchiveBulkForm(c); err != nil {
		return err
	}
	out, err := c.Do(op)
	if err != nil {
		return err
	}
	switch value := out.(type) {
	case RestoreBulkOut:
		c.Toast(fmt.Sprintf("Restored %d, skipped %d", value.Restored, value.Skipped))
	case DeleteBulkOut:
		c.Toast(fmt.Sprintf("Deleted %d, skipped %d", value.Deleted, value.Skipped))
	default:
		return fmt.Errorf("%s returned %T", op, out)
	}
	// A JSON client has already had its request body consumed by the bulk op. Preserve the original
	// JSON response contract instead of binding that body again for the refreshed HTML fragment.
	if strings.HasPrefix(c.R.Header.Get("Content-Type"), "application/json") {
		return writeArchiveJSON(c, out)
	}
	return renderArchive(c, false)
}

func writeArchiveJSON(c *web.Ctx, out any) error {
	c.W.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(c.W).Encode(out)
}
