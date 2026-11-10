package cardextras

import (
	"database/sql"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

type CommentItem struct {
	ID        string  `json:"id"`
	CardID    string  `json:"cardId"`
	UserID    *string `json:"userId"`
	UserName  *string `json:"userName"`
	Text      string  `json:"text"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt *string `json:"updatedAt"`
	CanEdit   bool    `json:"canEdit"`
	CanDelete bool    `json:"canDelete"`
}

// CommentsOut is the comment.list result.
type CommentsOut struct {
	CanComment bool          `json:"canComment"`
	Total      int           `json:"total"`
	Items      []CommentItem `json:"items"`
}

type CommentsIn struct {
	CardID string `path:"cardId"`
	Before string `query:"before" desc:"Comment id cursor: return comments older than this one"`
	Limit  int    `query:"limit" desc:"Page size (default 50, max 200)"`
}
type CommentCreateIn struct {
	CardID string `path:"cardId"`
	Text   string `json:"text" required:"true" desc:"Comment text (markdown)"`
}
type CommentUpdateIn struct {
	ID   string `path:"id"`
	Text string `json:"text" required:"true"`
}

const commentSelect = `SELECT c.id,c.card_id,c.user_id,u.name,c.text,c.created_at,c.updated_at FROM comments c LEFT JOIN users u ON u.id=c.user_id`

func commentScanner(e *ops.Env, level authz.Level) func(*sql.Rows) (CommentItem, error) {
	return func(r *sql.Rows) (CommentItem, error) {
		var c CommentItem
		var uid, un, up sql.NullString
		err := r.Scan(&c.ID, &c.CardID, &uid, &un, &c.Text, &c.CreatedAt, &up)
		c.UserID, c.UserName, c.UpdatedAt = strp(uid), strp(un), strp(up)
		mine := e.User != nil && c.UserID != nil && *c.UserID == e.User.UserID
		c.CanEdit = mine && level >= authz.Comment
		c.CanDelete = (mine && level >= authz.Comment) || level >= authz.Manage
		return c, err
	}
}

// commentRef loads a comment's card/board/author for permission checks.
func commentRef(e *ops.Env, id string) (cardID, boardID string, author sql.NullString, err error) {
	if e.DB.QueryRowContext(e.Ctx, `SELECT c.card_id,k.board_id,c.user_id FROM comments c JOIN cards k ON k.id=c.card_id WHERE c.id=?`, id).Scan(&cardID, &boardID, &author) != nil {
		return "", "", author, ops.NotFound("comment")
	}
	if authz.BoardLevel(e, boardID) == authz.None {
		return "", "", author, ops.NotFound("comment")
	}
	return
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func init() {
	ops.Register(ops.Op{Name: "comment.list", Summary: "List a card's comments, newest first", Method: "GET", Path: "/cards/{cardId}/comments",
		Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/comments"}, func(e *ops.Env, in CommentsIn) (CommentsOut, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.View)
		if err != nil {
			return CommentsOut{}, err
		}
		level := authz.BoardLevel(e, board)
		limit := in.Limit
		if limit <= 0 || limit > 200 {
			limit = 50
		}
		q, args := commentSelect+` WHERE c.card_id=?`, []any{in.CardID}
		if in.Before != "" {
			q += ` AND (c.created_at,c.id) < (SELECT created_at,id FROM comments WHERE id=?)`
			args = append(args, in.Before)
		}
		q += ` ORDER BY c.created_at DESC, c.id DESC LIMIT ?`
		args = append(args, limit)
		items, err := query(e.Ctx, e.DB, commentScanner(e, level), q, args...)
		if err != nil {
			return CommentsOut{}, err
		}
		out := CommentsOut{CanComment: e.User != nil && level >= authz.Comment, Items: items}
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT comments_total FROM cards WHERE id=?`, in.CardID).Scan(&out.Total)
		return out, nil
	})

	ops.Register(ops.Op{Name: "comment.create", Summary: "Comment on a card (requires comment permission)", Method: "POST", Path: "/cards/{cardId}/comments",
		UI: "POST /ui/cards/{cardId}/comments"}, func(e *ops.Env, in CommentCreateIn) (CommentItem, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Comment)
		if err != nil {
			return CommentItem{}, err
		}
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return CommentItem{}, ops.Invalid("text is required")
		}
		if len(text) > 1<<20 {
			return CommentItem{}, ops.Invalid("text too long")
		}
		id := db.NewID()
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES(?,?,?,?,?)`, id, in.CardID, e.User.UserID, text, db.Now()); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET comments_total=(SELECT COUNT(*) FROM comments WHERE card_id=?) WHERE id=?`, in.CardID, in.CardID)
			return err
		})
		if err != nil {
			return CommentItem{}, err
		}
		emit(e, "commentCreate", board, in.CardID, "comment.created", map[string]any{"commentId": id, "text": clip(text, 2000)})
		items, err := query(e.Ctx, e.DB, commentScanner(e, authz.BoardLevel(e, board)), commentSelect+` WHERE c.id=?`, id)
		if err != nil || len(items) == 0 {
			return CommentItem{}, err
		}
		return items[0], nil
	})

	ops.Register(ops.Op{Name: "comment.update", Summary: "Edit a comment (author only)", Method: "PATCH", Path: "/comments/{id}",
		UI: "POST /ui/comments/{id}"}, func(e *ops.Env, in CommentUpdateIn) (CommentItem, error) {
		cardID, board, author, err := commentRef(e, in.ID)
		if err != nil {
			return CommentItem{}, err
		}
		level := authz.BoardLevel(e, board)
		if !author.Valid || author.String != e.User.UserID {
			return CommentItem{}, ops.Forbidden()
		}
		if level < authz.Comment {
			return CommentItem{}, ops.Forbidden()
		}
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return CommentItem{}, ops.Invalid("text is required")
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE comments SET text=?,updated_at=? WHERE id=?`, text, db.Now(), in.ID); err != nil {
			return CommentItem{}, err
		}
		emit(e, "commentUpdate", board, cardID, "comment.updated", map[string]any{"commentId": in.ID, "text": clip(text, 2000)})
		items, err := query(e.Ctx, e.DB, commentScanner(e, level), commentSelect+` WHERE c.id=?`, in.ID)
		if err != nil || len(items) == 0 {
			return CommentItem{}, err
		}
		return items[0], nil
	})

	ops.Register(ops.Op{Name: "comment.delete", Summary: "Delete a comment (author, or a project manager/admin)", Method: "DELETE", Path: "/comments/{id}",
		UI: "POST /ui/comments/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		cardID, board, author, err := commentRef(e, in.ID)
		if err != nil {
			return DeletedOut{}, err
		}
		level := authz.BoardLevel(e, board)
		mine := author.Valid && author.String == e.User.UserID && level >= authz.Comment
		if !mine && level < authz.Manage {
			return DeletedOut{}, ops.Forbidden()
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM comments WHERE id=?`, in.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(e.Ctx, `UPDATE cards SET comments_total=(SELECT COUNT(*) FROM comments WHERE card_id=?) WHERE id=?`, cardID, cardID)
			return err
		})
		if err != nil {
			return DeletedOut{}, err
		}
		emit(e, "commentDelete", board, cardID, "comment.deleted", map[string]any{"commentId": in.ID})
		return DeletedOut{in.ID, cardID}, nil
	})
}
