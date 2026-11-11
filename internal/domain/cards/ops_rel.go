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
}

type LabelRemoveIn struct {
	ID      string `path:"id"`
	LabelID string `path:"labelId"`
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
	ops.Register(ops.Op{Name: "card.subscribe", Summary: "Subscribe to card notifications", Method: "PUT", Path: "/cards/{id}/subscription",
		UI: "POST /ui/cards/{id}/subscribe"}, func(e *ops.Env, in IDIn) (SubscribeOut, error) {
		if _, err := viewCard(e, in.ID); err != nil {
			return SubscribeOut{}, err
		}
		subscribe(e.Ctx, e.DB, in.ID, e.User.UserID, true)
		return SubscribeOut{in.ID, true}, nil
	})
	ops.Register(ops.Op{Name: "card.unsubscribe", Summary: "Unsubscribe from card notifications", Method: "DELETE", Path: "/cards/{id}/subscription",
		UI: "POST /ui/cards/{id}/unsubscribe"}, func(e *ops.Env, in IDIn) (SubscribeOut, error) {
		if _, err := viewCard(e, in.ID); err != nil {
			return SubscribeOut{}, err
		}
		_, err := e.DB.ExecContext(e.Ctx, `DELETE FROM card_subscriptions WHERE card_id=? AND user_id=?`, in.ID, e.User.UserID)
		return SubscribeOut{in.ID, false}, err
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
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	if !boardMemberIDs(e.Ctx, e.DB, boardID)[in.UserID] {
		return Card{}, ops.Invalid("user is not a member of this board")
	}
	res, err := e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO card_memberships(card_id,user_id) VALUES(?,?)`, in.ID, in.UserID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		subscribe(e.Ctx, e.DB, in.ID, in.UserID, false)
		_, _ = e.DB.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), in.ID)
		c, _ := loadCard(e, e.DB, in.ID)
		emit(e, "cardMembershipCreate", "card.member.added", boardID, in.ID, map[string]any{"userId": in.UserID, "card": c})
	}
	return loadCard(e, e.DB, in.ID)
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
		c, _ := loadCard(e, e.DB, in.ID)
		emit(e, "cardMembershipDelete", "card.member.removed", boardID, in.ID, map[string]any{"userId": in.UserID, "card": c})
	}
	return loadCard(e, e.DB, in.ID)
}

func labelAdd(e *ops.Env, in LabelIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
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
	c, err := loadCard(e, e.DB, in.ID)
	if err == nil {
		emit(e, "cardLabelCreate", "card.label.added", boardID, in.ID, map[string]any{"labelId": in.LabelID, "card": c})
	}
	return c, err
}

func labelRemove(e *ops.Env, in LabelRemoveIn) (Card, error) {
	boardID, err := editCard(e, in.ID)
	if err != nil {
		return Card{}, err
	}
	res, err := e.DB.ExecContext(e.Ctx, `DELETE FROM card_labels WHERE card_id=? AND label_id=?`, in.ID, in.LabelID)
	if err != nil {
		return Card{}, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, _ = e.DB.ExecContext(e.Ctx, `UPDATE cards SET updated_at=? WHERE id=?`, db.Now(), in.ID)
		c, _ := loadCard(e, e.DB, in.ID)
		emit(e, "cardLabelDelete", "card.label.removed", boardID, in.ID, map[string]any{"labelId": in.LabelID, "card": c})
	}
	return loadCard(e, e.DB, in.ID)
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
}

func init() {
	ops.Register(ops.Op{Name: "card.options", Summary: "Lists, board members and labels available to a card (drives the card modal pickers)", Method: "GET", Path: "/cards/{id}/options",
		UI: "GET /ui/cards/{id}"}, cardOptions)
}

func cardOptions(e *ops.Env, in IDIn) (Options, error) {
	boardID, err := viewCard(e, in.ID)
	if err != nil {
		return Options{}, err
	}
	lvl := authz.BoardLevel(e, boardID)
	o := Options{CanEdit: lvl >= authz.Edit, CanComment: lvl >= authz.Comment, Lists: []ListOpt{}, Members: []UserOpt{}, Labels: []LabelOpt{}}
	var listID string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT b.name,b.project_id,COALESCE(p.name,''),COALESCE(l.name,l.type),l.type,l.id FROM boards b JOIN cards c ON c.board_id=b.id JOIN lists l ON l.id=c.list_id LEFT JOIN projects p ON p.id=b.project_id WHERE c.id=?`, in.ID).
		Scan(&o.BoardName, &o.ProjectID, &o.ProjectName, &o.ListName, &o.ListType, &listID)
	o.CardMembers, o.CardLabels = []UserOpt{}, []LabelOpt{}
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT u.id,u.name,COALESCE(u.username,'') FROM card_memberships m JOIN users u ON u.id=m.user_id WHERE m.card_id=? ORDER BY u.name`, in.ID); err == nil {
		for rows.Next() {
			var u UserOpt
			_ = rows.Scan(&u.ID, &u.Name, &u.Username)
			o.CardMembers = append(o.CardMembers, u)
		}
		rows.Close()
	}
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT l.id,COALESCE(l.name,''),l.color,COALESCE(l.group_id,''),'',0 FROM card_labels cl JOIN labels l ON l.id=cl.label_id WHERE cl.card_id=? ORDER BY l.position`, in.ID); err == nil {
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
	// lists on this board plus sibling boards of the project the caller can edit
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT l.id,b.id,b.name,COALESCE(l.name,l.type),l.type FROM lists l JOIN boards b ON b.id=l.board_id
		WHERE b.project_id=? AND l.type IN ('active','closed') AND b.is_template=0 ORDER BY (b.id=?) DESC, b.position, l.position`, o.ProjectID, boardID)
	if err == nil {
		seen := map[string]bool{}
		for rows.Next() {
			var l ListOpt
			_ = rows.Scan(&l.ID, &l.BoardID, &l.BoardName, &l.Name, &l.Type)
			if _, ok := seen[l.BoardID]; !ok {
				seen[l.BoardID] = authz.BoardLevel(e, l.BoardID) >= authz.Edit
			}
			if seen[l.BoardID] {
				o.Lists = append(o.Lists, l)
			}
		}
		rows.Close()
	}
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
