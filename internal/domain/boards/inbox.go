package boards

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// The board Inbox: when a board has has_inbox on, its kanban view starts with a pinned Inbox column holding the
// board's drafts (cards with is_draft=1, owned by the cards domain). Capturing goes through op card.draft.create,
// "Move to column" and drag-to-column through op card.draft.publish; this file only renders.

// Draft is one inbox item as the panel shows it.
type Draft struct {
	ID, Name       string
	CreatorID      string // empty when the creator account is gone
	CreatorName    string
	CreatorIsAgent bool
	HasDescription bool
	CreatedAt      string // RFC3339
	Age            string // compact: "now", "5m", "3h", "2d", "Mar 4"
	AgeTitle       string // full timestamp for the tooltip
}

// LoadDrafts returns a board's drafts, oldest first, in a constant number of queries (no authz: the caller has
// already authorized the board). The cards domain owns the drafts and installs it from its init; nil (cards not
// linked) means no drafts.
var LoadDrafts func(e *ops.Env, boardID string) ([]Draft, error)

// draftAge formats a compact age for a draft tile.
func draftAge(created string, now time.Time) (string, string) {
	t, ok := parseTime(created)
	if !ok {
		return "", ""
	}
	full := t.Local().Format("Jan 2, 2006 15:04")
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now", full
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes())), full
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours())), full
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24)), full
	}
	return t.Local().Format("Jan 2"), full
}

// showInbox decides whether the caller sees the Inbox of a board: the board has it on, the page is not the
// read-only public page, and the caller has real access (admin, project manager or a member). Someone who can
// only see the board because it is public never sees drafts: they are ideas not yet on the board.
// pd.Caps (authz.Board) already carries both: CanSeeInbox is cleared on the public page.
func showInbox(pd *PageData) bool {
	return pd.Board.HasInbox && LoadDrafts != nil && pd.Caps.CanSeeInbox
}

// loadInbox fills the inbox part of the page data (one query).
func loadInbox(e *ops.Env, pd *PageData, readOnly bool) error {
	if !showInbox(pd) {
		return nil
	}
	ds, err := LoadDrafts(e, pd.Board.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	for i := range ds {
		ds[i].Age, ds[i].AgeTitle = draftAge(ds[i].CreatedAt, now)
	}
	pd.HasInbox, pd.Drafts = true, ds
	return nil
}

// InboxData feeds the inbox fragments (the panel and new draft tiles).
type InboxData struct {
	P      *PageData
	Drafts []Draft
}

// inboxPage loads the page data the panel needs (kanban, no filter) after checking the caller may see the inbox.
func inboxPage(c *web.Ctx, boardID string) (*PageData, error) {
	d, err := ops.Typed[BoardDetail](c.Env, "board.get", BoardGetIn{ID: boardID})
	if err != nil {
		return nil, err
	}
	pd, err := loadPage(c.Env, d.Board, url.Values{"view": {"kanban"}}, false)
	if err != nil {
		return nil, err
	}
	if !pd.HasInbox {
		return nil, ops.NotFound("inbox")
	}
	return pd, nil
}

func registerInbox() {
	// The panel alone (used after toggling the inbox, and as a lazy fragment).
	web.Route("GET /ui/boards/{boardId}/inbox", func(c *web.Ctx) error {
		pd, err := inboxPage(c, c.R.PathValue("boardId"))
		if err != nil {
			return err
		}
		return c.RenderBlock("boards_board.html", "boards_inbox_frag", pd)
	})

	// Capture one or several drafts (one per line). Like the column composer: answers with just the new draft
	// tiles and never opens the card.
	web.Route("POST /ui/boards/{boardId}/inbox", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		boardID := c.R.PathValue("boardId")
		if err := authz.RequireBoard(c.Env, boardID, authz.Edit); err != nil {
			return err
		}
		names := splitNames(c.R.Form["name"])
		if len(names) == 0 {
			if !c.IsHTMX() {
				return backToBoard(c, boardID)
			}
			c.W.WriteHeader(http.StatusNoContent)
			return nil
		}
		var ids []string
		var failure error
		for _, n := range names {
			out, err := CallByForm(c.Env, "card.draft.create", url.Values{"boardId": {boardID}, "name": {n}})
			if err != nil {
				failure = err
				break
			}
			ids = append(ids, idOf(out))
		}
		if len(ids) == 0 && failure != nil {
			return failure
		}
		if !c.IsHTMX() {
			return backToBoard(c, boardID)
		}
		pd, err := inboxPage(c, boardID)
		if err != nil {
			return err
		}
		want := map[string]bool{}
		for _, id := range ids {
			want[id] = true
		}
		var fresh []Draft
		for _, d := range pd.Drafts {
			if want[d.ID] {
				fresh = append(fresh, d)
			}
		}
		detail := map[string]any{"listId": "inbox", "ids": ids, "count": len(ids)}
		if failure != nil {
			detail["error"] = "Some ideas could not be added"
		}
		trigger(c, "boards:added", detail)
		return c.RenderBlock("boards_board.html", "boards_drafts", InboxData{P: pd, Drafts: fresh})
	})

	// Turn the inbox on or off (op card.inbox.set), then refresh the board.
	web.Route("POST /ui/boards/{boardId}/inbox/settings", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		boardID := c.R.PathValue("boardId")
		if _, err := CallByForm(c.Env, "card.inbox.set", url.Values{"boardId": {boardID}, "enabled": {c.R.Form.Get("enabled")}}); err != nil {
			return err
		}
		if !c.IsHTMX() {
			return backToBoard(c, boardID)
		}
		return refresh(c)
	})

	// "Move to column": publish a draft into a list of this board (op card.draft.publish, appended at the end).
	// Answers with the new card tile so the page can put it into its column; boards:published carries where.
	web.Route("POST /ui/boards/{boardId}/inbox/publish", func(c *web.Ctx) error {
		if err := c.R.ParseForm(); err != nil {
			return ops.Invalid("bad form")
		}
		boardID := c.R.PathValue("boardId")
		if err := authz.RequireBoard(c.Env, boardID, authz.Edit); err != nil {
			return err
		}
		id, listID := c.R.Form.Get("id"), c.R.Form.Get("listId")
		l, err := GetList(c.R.Context(), c.Env.DB, listID)
		if err != nil || l.BoardID != boardID {
			return ops.NotFound("list")
		}
		var n int
		_ = c.Env.DB.QueryRowContext(c.R.Context(), `SELECT COUNT(*) FROM cards WHERE id=? AND board_id=? AND is_draft=1`, id, boardID).Scan(&n)
		if n == 0 {
			return ops.NotFound("draft")
		}
		if _, err := CallByForm(c.Env, "card.draft.publish", url.Values{"id": {id}, "listId": {listID}}); err != nil {
			return err
		}
		if !c.IsHTMX() {
			return backToBoard(c, boardID)
		}
		pd, err := shell(c, boardID)
		if err != nil {
			return err
		}
		tiles, err := tilesFor(c, pd, []string{id})
		if err != nil {
			return err
		}
		name := ""
		if l.Name != nil {
			name = *l.Name
		}
		trigger(c, "boards:published", map[string]any{"id": id, "listId": listID, "listName": name})
		return c.RenderBlock("boards_board.html", "boards_tiles", TilesData{P: pd, Tiles: tiles})
	})
}

// draftDrop publishes a draft dropped onto a column (drag from the Inbox), at the dropped position.
func draftDrop(c *web.Ctx, cardID, listID string, pos float64) error {
	_, err := CallByForm(c.Env, "card.draft.publish", url.Values{"id": {cardID}, "listId": {listID}, "position": {fmt.Sprint(pos)}})
	return err
}
