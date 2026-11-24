package cards

import (
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

type MemberIn struct {
	ID     string `path:"id"`
	UserID string `json:"userId" required:"true"`
}

type MemberRemoveIn struct {
	ID     string `path:"id"`
	UserID string `path:"userId"`
}

type LabelIn struct {
	ID      string `path:"id"`
	LabelID string `json:"labelId" required:"true"`
	BoardID string `json:"boardId" desc:"the board whose label it is (labels belong to a board); required when the card is on several boards you can see"`
}

type LabelRemoveIn struct {
	ID      string `path:"id"`
	LabelID string `path:"labelId"`
	BoardID string `query:"boardId" desc:"the board whose label it is; required when the card is on several boards you can see"`
}

type SubscribeOut struct {
	CardID     string `json:"cardId"`
	Subscribed bool   `json:"subscribed"`
}

type ReadOut struct {
	Marked int64 `json:"marked"`
}

// MarkNotificationsRead is the hook that marks a user's notifications for a card
// as read. The notify domain may replace it; the default updates the shared table.
var MarkNotificationsRead = func(e *ops.Env, cardID string) (int64, error) {
	r, err := e.DB.ExecContext(e.Ctx, `UPDATE notifications SET is_read=1 WHERE user_id=? AND card_id=? AND is_read=0`, e.User.UserID, cardID)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func init() {
	ops.Register(ops.Op{Name: "card.member.add", Summary: "Add a board member to a card", Method: "POST", Path: "/cards/{id}/members",
		UI: "POST /ui/cards/{id}/members"}, memberAdd)
	ops.Register(ops.Op{Name: "card.member.remove", Summary: "Remove a member from a card", Method: "DELETE", Path: "/cards/{id}/members/{userId}",
		UI: "POST /ui/cards/{id}/members/{userId}/remove"}, memberRemove)
	ops.Register(ops.Op{Name: "card.label.add", Summary: "Add a label to a card (single-select groups replace the group's other label)", Method: "POST", Path: "/cards/{id}/labels",
		UI: "POST /ui/cards/{id}/labels"}, labelAdd)
	ops.Register(ops.Op{Name: "card.label.remove", Summary: "Remove a label from a card", Method: "DELETE", Path: "/cards/{id}/labels/{labelId}",
		UI: "POST /ui/cards/{id}/labels/{labelId}/remove"}, labelRemove)
	// Watch toggles emit no event: an activity row would tell everyone on the board who watches what.
	ops.Register(ops.Op{Name: "card.subscribe", Summary: "Watch a card: get notified when it is moved, closed or re-opened, commented on or its due date changes (also clears a mute)", Method: "PUT", Path: "/cards/{id}/subscription",
		UI: "POST /ui/cards/{id}/subscribe"}, func(e *ops.Env, in IDIn) (SubscribeOut, error) {
		if _, err := viewCard(e, in.ID); err != nil {
			return SubscribeOut{}, err
		}
		return SubscribeOut{in.ID, true}, watchCard(e.Ctx, e.DB, in.ID, e.User.UserID)
	})
	ops.Register(ops.Op{Name: "card.unsubscribe", Summary: "Stop watching a card and mute it: no notifications about it even through a watched column or board, and creating, commenting or being added never re-watches it (card.subscribe undoes)", Method: "DELETE", Path: "/cards/{id}/subscription",
		UI: "POST /ui/cards/{id}/unsubscribe"}, func(e *ops.Env, in IDIn) (SubscribeOut, error) {
		if _, err := viewCard(e, in.ID); err != nil {
			return SubscribeOut{}, err
		}
		return SubscribeOut{in.ID, false}, muteCard(e.Ctx, e.DB, in.ID, e.User.UserID)
	})
	ops.Register(ops.Op{Name: "card.notifications.read", Summary: "Mark all of the caller's notifications for this card as read", Method: "POST", Path: "/cards/{id}/notifications/read",
		UI: "POST /ui/cards/{id}/notifications/read"}, func(e *ops.Env, in IDIn) (ReadOut, error) {
		if _, err := viewCard(e, in.ID); err != nil {
			return ReadOut{}, err
		}
		n, err := MarkNotificationsRead(e, in.ID)
		return ReadOut{n}, err
	})
}

func memberAdd(e *ops.Env, in MemberIn) (Card, error) {
	a, err := authz.CardEditAccess(e, in.ID, "")
	if err != nil {
		return Card{}, err
	}
	boardID := a.BoardID
	member := false // a member of a board the card is on (one the caller can see)
	for b := range a.Boards {
		if boardMemberIDs(e.Ctx, e.DB, b)[in.UserID] {
			member = true
			break
		}
	}
	if !member {
		return Card{}, ops.Invalid("user is not a member of this board")
	}
	res, err := e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO card_memberships(card_id,user_id) VALUES(?,?)`, in.ID, in.UserID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		autoWatch(e.Ctx, e.DB, in.ID, in.UserID, "") // being added to a card watches it (never over a mute; not for agents)
		_, _ = e.DB.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), in.ID)
		c, _ := loadCardOn(e, e.DB, in.ID, boardID)
		emit(e, "cardMembershipCreate", "card.member.added", boardID, in.ID, map[string]any{"userId": in.UserID, "card": c})
	}
	return loadCardOn(e, e.DB, in.ID, boardID)
}

func memberRemove(e *ops.Env, in MemberRemoveIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	res, err := e.DB.ExecContext(e.Ctx, `DELETE FROM card_memberships WHERE card_id=? AND user_id=?`, in.ID, in.UserID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = e.DB.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), in.ID)
		c, _ := loadCardOn(e, e.DB, in.ID, boardID)
		emit(e, "cardMembershipDelete", "card.member.removed", boardID, in.ID, map[string]any{"userId": in.UserID, "card": c})
	}
	return loadCardOn(e, e.DB, in.ID, boardID)
}

func labelAdd(e *ops.Env, in LabelIn) (Card, error) {
	boardID, err := labelBoard(e, in.ID, in.BoardID)
	if err != nil {
		return Card{}, err
	}
	if !labelAllowed(e.Ctx, e.DB, boardID, in.LabelID) {
		return Card{}, ops.NotFound("label")
	}
	err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var group sql.NullString
		var single bool
		_ = tx.QueryRowContext(e.Ctx, `SELECT l.group_id,COALESCE(g.single_select,0) FROM labels l LEFT JOIN label_groups g ON g.id=l.group_id WHERE l.id=?`, in.LabelID).Scan(&group, &single)
		if group.Valid && single {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM card_labels WHERE card_id=? AND label_id<>? AND label_id IN (SELECT id FROM labels WHERE group_id=?)`,
				in.ID, in.LabelID, group.String); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(e.Ctx, `INSERT OR IGNORE INTO card_labels(card_id,label_id) VALUES(?,?)`, in.ID, in.LabelID)
		if err == nil {
			_, err = tx.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), in.ID)
		}
		return err
	})
	if err != nil {
		return Card{}, err
	}
	c, err := loadCardOn(e, e.DB, in.ID, boardID)
	if err == nil {
		// a label is board (or project) vocabulary: the event reaches only the boards that show it
		emitScoped(e, "", "cardLabelCreate", "card.label.added", boardID, in.ID, map[string]any{"labelId": in.LabelID, "card": c}, labelBoards(e, in.ID, in.LabelID))
	}
	return c, err
}

// labelBoard is the board of a label write: the card-level edit rule, then the placement (boardId, or the only one
// the caller can see) on which the caller may edit (labels are board vocabulary, so a label write is that board's).
func labelBoard(e *ops.Env, cardID, boardHint string) (string, error) {
	a, err := authz.CardEditAccess(e, cardID, boardHint)
	if err != nil {
		return "", err
	}
	b, err := placementBoard(a, boardHint, "labels belong to a board")
	if err != nil {
		return "", err
	}
	if !a.EditableOn(b) {
		return "", ops.Forbidden()
	}
	return b, nil
}

func labelRemove(e *ops.Env, in LabelRemoveIn) (Card, error) {
	boardID, err := labelBoard(e, in.ID, in.BoardID)
	if err != nil {
		return Card{}, err
	}
	if placementCount(e.Ctx, e.DB, in.ID) > 1 { // on several boards: only a label of this board (another board's stays)
		var n int
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM labels l WHERE l.id=?1 AND `+db.LabelOnBoard("l", "?2"), in.LabelID, boardID).Scan(&n)
		if n == 0 {
			return Card{}, ops.NotFound("label")
		}
	}
	res, err := e.DB.ExecContext(e.Ctx, `DELETE FROM card_labels WHERE card_id=? AND label_id=?`, in.ID, in.LabelID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = e.DB.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), in.ID)
		c, _ := loadCardOn(e, e.DB, in.ID, boardID)
		emitScoped(e, "", "cardLabelDelete", "card.label.removed", boardID, in.ID, map[string]any{"labelId": in.LabelID, "card": c}, labelBoards(e, in.ID, in.LabelID))
	}
	return loadCardOn(e, e.DB, in.ID, boardID)
}

// ---- options for the card modal (lists, members, labels the caller may pick from)

type ListOpt struct {
	ID        string `json:"id"`
	BoardID   string `json:"boardId"`
	BoardName string `json:"boardName"`
	Name      string `json:"name"`
	Type      string `json:"type"`
}
type UserOpt struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}
type LabelOpt struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	GroupID      string `json:"groupId"`
	GroupName    string `json:"groupName"`
	SingleSelect bool   `json:"singleSelect"`
}
type Options struct {
	CanEdit     bool       `json:"canEdit"`
	CanEditHere bool       `json:"canEditHere" desc:"you may change the card's placement on the context board: move it between that board's columns (lists) and toggle that board's labels (Edit there, or the worker rule); canEdit is the card itself (any board you can edit it on)"`
	CanComment  bool       `json:"canComment"`
	BoardName   string     `json:"boardName"`
	ListName    string     `json:"listName"`
	ListType    string     `json:"listType"`
	ProjectID   string     `json:"projectId"`
	ProjectName string     `json:"projectName"`
	Lists       []ListOpt  `json:"lists"`
	Members     []UserOpt  `json:"members"`
	CardMembers []UserOpt  `json:"cardMembers"`
	CardLabels  []LabelOpt `json:"cardLabels"`
	Labels      []LabelOpt `json:"labels"`
	Watch       WatchState `json:"watch" desc:"the caller's own watch state for this card"`
}

// WatchState is the caller's watch state of a card (per viewer; never part of events or payloads).
type WatchState struct {
	Via   string `json:"via,omitempty" desc:"why you hear about this card: card (you watch it) | list (you watch its column) | board (you watch the board); absent = not watching"`
	Muted bool   `json:"muted" desc:"you muted this card: no notifications about it, even through a watched column or board"`
}

func init() {
	ops.Register(ops.Op{Name: "card.options", Summary: "Lists, board members and labels available to a card, and the caller's watch state (drives the card modal pickers)", Method: "GET", Path: "/cards/{id}/options",
		UI: "GET /ui/cards/{id}"}, cardOptions)
}

// OptionsIn is card.options' input: the card and its context board.
type OptionsIn struct {
	ID      string `path:"id" desc:"card id"`
	BoardID string `query:"boardId" desc:"context board when the card is on several boards (its columns, members and labels); default as card.get"`
}

func cardOptions(e *ops.Env, in OptionsIn) (Options, error) {
	a, err := authz.CardAccessFor(e, in.ID, in.BoardID)
	if err != nil {
		return Options{}, err
	}
	boardID := a.BoardID
	cc := authz.Card(e, in.ID) // the same predicates as the card ops (worker rule included)
	o := Options{CanEdit: cc.CanEdit, CanEditHere: cc.CanEdit && a.EditableOn(boardID), CanComment: cc.CanComment, Lists: []ListOpt{}, Members: []UserOpt{}, Labels: []LabelOpt{}}
	var listID string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT b.name,b.project_id,COALESCE(p.name,''),COALESCE(l.name,l.type),l.type,l.id FROM card_placements c JOIN boards b ON b.id=c.board_id JOIN lists l ON l.id=c.list_id LEFT JOIN projects p ON p.id=b.project_id WHERE c.card_id=? AND c.board_id=?`, in.ID, boardID).
		Scan(&o.BoardName, &o.ProjectID, &o.ProjectName, &o.ListName, &o.ListType, &listID)
	if e.User != nil {
		var muted *bool
		var list, board bool
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT (SELECT muted FROM card_subscriptions WHERE card_id=?1 AND user_id=?2),
			EXISTS(SELECT 1 FROM list_subscriptions WHERE list_id=?3 AND user_id=?2), EXISTS(SELECT 1 FROM board_subscriptions WHERE board_id=?4 AND user_id=?2)`,
			in.ID, e.User.UserID, listID, boardID).Scan(&muted, &list, &board)
		switch {
		case muted != nil && *muted:
			o.Watch.Muted = true
		case muted != nil:
			o.Watch.Via = "card"
		case list:
			o.Watch.Via = "list"
		case board:
			o.Watch.Via = "board"
		}
	}
	o.CardMembers, o.CardLabels = []UserOpt{}, []LabelOpt{}
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT u.id,u.name,COALESCE(u.username,'') FROM card_memberships m JOIN users u ON u.id=m.user_id WHERE m.card_id=? ORDER BY u.name`, in.ID); err == nil {
		for rows.Next() {
			var u UserOpt
			_ = rows.Scan(&u.ID, &u.Name, &u.Username)
			o.CardMembers = append(o.CardMembers, u)
		}
		rows.Close()
	}
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT l.id,COALESCE(l.name,''),l.color,COALESCE(l.group_id,''),'',0 FROM card_labels cl JOIN labels l ON l.id=cl.label_id
		WHERE cl.card_id=?1 AND `+db.LabelOnBoard("l", "?2")+` ORDER BY l.position`, in.ID, boardID); err == nil {
		for rows.Next() {
			var l LabelOpt
			_ = rows.Scan(&l.ID, &l.Name, &l.Color, &l.GroupID, &l.GroupName, &l.SingleSelect)
			o.CardLabels = append(o.CardLabels, l)
		}
		rows.Close()
	}
	if !o.CanEdit {
		return o, nil
	}
	// lists on this board plus sibling boards of the project the caller can edit (a move of the context placement:
	// only when the caller may change it here). Another board the card is already on is left out: moving this
	// placement there would be a conflict (the board switcher shows the card on that board instead).
	if !o.CanEditHere {
		return optionsPeople(e, o, boardID)
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT l.id,b.id,b.name,COALESCE(l.name,l.type),l.type FROM lists l JOIN boards b ON b.id=l.board_id
		WHERE b.project_id=? AND l.type IN ('active','closed') AND b.is_template=0 ORDER BY (b.id=?) DESC, b.position, l.position`, o.ProjectID, boardID)
	if err == nil {
		seen := map[string]bool{}
		for rows.Next() {
			var l ListOpt
			_ = rows.Scan(&l.ID, &l.BoardID, &l.BoardName, &l.Name, &l.Type)
			if _, ok := seen[l.BoardID]; !ok {
				_, placed := a.Boards[l.BoardID]
				seen[l.BoardID] = (l.BoardID == boardID || !placed) && authz.BoardLevel(e, l.BoardID) >= authz.Edit
			}
			if seen[l.BoardID] {
				o.Lists = append(o.Lists, l)
			}
		}
		rows.Close()
	}
	return optionsPeople(e, o, boardID)
}

// optionsPeople adds the board members and the board's labels a card editor picks from (card.options).
func optionsPeople(e *ops.Env, o Options, boardID string) (Options, error) {
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT u.id,u.name,COALESCE(u.username,'') FROM users u WHERE u.deleted_at IS NULL AND u.is_deactivated=0 AND (
		u.id IN (SELECT user_id FROM board_memberships WHERE board_id=?1 AND user_id IS NOT NULL) OR
		u.id IN (SELECT gm.user_id FROM user_group_members gm JOIN board_memberships bm ON bm.group_id=gm.group_id WHERE bm.board_id=?1)) ORDER BY u.name`, boardID); err == nil {
		for rows.Next() {
			var u UserOpt
			_ = rows.Scan(&u.ID, &u.Name, &u.Username)
			o.Members = append(o.Members, u)
		}
		rows.Close()
	}
	if rows, err := e.DB.QueryContext(e.Ctx, labelsOfBoard, boardID, o.ProjectID, o.ProjectID); err == nil {
		for rows.Next() {
			var l LabelOpt
			_ = rows.Scan(&l.ID, &l.Name, &l.Color, &l.GroupID, &l.GroupName, &l.SingleSelect)
			o.Labels = append(o.Labels, l)
		}
		rows.Close()
	}
	return o, nil
}
