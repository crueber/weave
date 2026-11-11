package cardextras

import (
	"bytes"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/imgguard"
	"github.com/crueber/weave/internal/ops"
)

const (
	thumbMax  = 360
	maxUpload = 256 << 20
)

type AttachmentItem struct {
	ID           string  `json:"id"`
	CardID       string  `json:"cardId"`
	CardName     string  `json:"cardName,omitempty"`
	CreatorID    *string `json:"creatorUserId"`
	CreatorName  *string `json:"creatorName"`
	Type         string  `json:"type"` // file | link
	Name         string  `json:"name"`
	URL          *string `json:"url"`
	Mime         *string `json:"mime"`
	Size         int64   `json:"size"`
	IsImage      bool    `json:"isImage"`
	IsVideo      bool    `json:"isVideo"`
	Width        *int    `json:"width"`
	Height       *int    `json:"height"`
	IsCover      bool    `json:"isCover"`
	DownloadURL  string  `json:"downloadUrl,omitempty"`
	ThumbnailURL string  `json:"thumbnailUrl,omitempty"`
	CreatedAt    string  `json:"createdAt"`
}

type AttachmentCreateIn struct {
	CardID string    `path:"cardId"`
	File   *ops.File `json:"file" desc:"File to upload (multipart). Omit for a link attachment."`
	URL    string    `json:"url" desc:"Link URL (http/https) for a link attachment"`
	Name   string    `json:"name" desc:"Display name (defaults to the file name or URL host)"`
}
type AttachmentUpdateIn struct {
	ID   string `path:"id"`
	Name string `json:"name" required:"true"`
}
type AttachmentCoverIn struct {
	ID    string `path:"id"`
	Unset bool   `json:"unset" desc:"Remove this attachment as the card cover instead of setting it"`
}
type AttachmentDownloadIn struct {
	ID   string `path:"id"`
	Name string `path:"name" desc:"File name (cosmetic; the id decides)"`
}
type ThumbnailIn struct {
	ID string `path:"id"`
}
type MediaIn struct {
	BoardID string `path:"boardId"`
	Type    string `query:"type" desc:"Filter: all (default) | image | video | doc"`
}

const attSelect = `SELECT a.id,a.card_id,c.name,a.creator_user_id,u.name,a.type,a.name,a.url,a.mime,COALESCE(a.size,0),a.is_image,a.width,a.height,
	COALESCE(c.cover_attachment_id=a.id,0),a.created_at,a.file
	FROM attachments a JOIN cards c ON c.id=a.card_id LEFT JOIN users u ON u.id=a.creator_user_id`

func scanAtt(dir string) func(*sql.Rows) (AttachmentItem, error) {
	return func(r *sql.Rows) (AttachmentItem, error) {
		var a AttachmentItem
		var cr, cn, urlv, mime, file sql.NullString
		var w, h sql.NullInt64
		var img, cover int
		err := r.Scan(&a.ID, &a.CardID, &a.CardName, &cr, &cn, &a.Type, &a.Name, &urlv, &mime, &a.Size, &img, &w, &h, &cover, &a.CreatedAt, &file)
		a.CreatorID, a.CreatorName, a.URL, a.Mime = strp(cr), strp(cn), strp(urlv), strp(mime)
		a.IsImage, a.IsCover = img != 0, cover != 0
		if w.Valid {
			v := int(w.Int64)
			a.Width = &v
		}
		if h.Valid {
			v := int(h.Int64)
			a.Height = &v
		}
		if a.Mime != nil && strings.HasPrefix(*a.Mime, "video/") {
			a.IsVideo = true
		}
		if a.Type == "file" {
			a.DownloadURL = "/attachments/" + a.ID + "/download/" + url.PathEscape(a.Name)
			if a.IsImage && file.Valid {
				if _, err := os.Stat(filepath.Join(dir, "files", path.Dir(file.String), "thumb.jpg")); err == nil {
					a.ThumbnailURL = "/attachments/" + a.ID + "/thumbnail"
				}
			}
		}
		return a, err
	}
}

func getAttachment(e *ops.Env, id string) (AttachmentItem, error) {
	l, err := query(e.Ctx, e.DB, scanAtt(dataDir(e)), attSelect+` WHERE a.id=?`, id)
	if err != nil {
		return AttachmentItem{}, err
	}
	if len(l) == 0 {
		return AttachmentItem{}, ops.NotFound("attachment")
	}
	return l[0], nil
}

func safeName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '"' {
			return -1
		}
		return r
	}, n)
	n = strings.TrimSpace(n)
	if n == "" || n == "." || n == ".." {
		n = "file"
	}
	if len(n) > 200 {
		ext := filepath.Ext(n)
		if len(ext) > 20 {
			ext = ""
		}
		n = n[:200-len(ext)] + ext
	}
	return n
}

func addStorage(q querier, e *ops.Env, delta int64) error {
	_, err := q.ExecContext(e.Ctx, `INSERT INTO storage_usage(id,bytes) VALUES(1,MAX(0,?)) ON CONFLICT(id) DO UPDATE SET bytes=MAX(0,bytes+?)`, delta, delta)
	return err
}

// makeThumb decodes an image and returns a JPEG thumbnail (<=thumbMax px) plus original dimensions.
func makeThumb(data []byte) (thumb []byte, w, h int, ok bool) {
	cfg, _, err := imgguard.Config(data)
	if errors.Is(err, imgguard.ErrTooLarge) {
		return nil, cfg.Width, cfg.Height, true // valid image, too big to thumbnail safely
	}
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, 0, 0, false
	}
	w, h = cfg.Width, cfg.Height
	img, _, err := imgguard.Decode(data)
	if err != nil {
		return nil, w, h, true
	}
	tw, th := w, h
	if w > thumbMax || h > thumbMax {
		if w >= h {
			tw, th = thumbMax, max(1, h*thumbMax/w)
		} else {
			tw, th = max(1, w*thumbMax/h), thumbMax
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
	var buf bytes.Buffer
	if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}) != nil {
		return nil, w, h, true
	}
	return buf.Bytes(), w, h, true
}

func removeAttachmentFiles(e *ops.Env, file string) int64 {
	if file == "" {
		return 0
	}
	dir := filepath.Join(dataDir(e), "files", path.Dir(file))
	var total int64
	if ents, err := os.ReadDir(dir); err == nil {
		for _, en := range ents {
			if fi, err := en.Info(); err == nil {
				total += fi.Size()
			}
		}
	}
	_ = os.RemoveAll(dir)
	return total
}

// attachmentFile resolves a stored relative path safely under the files directory.
func attachmentPath(e *ops.Env, rel string) (string, bool) {
	base := filepath.Clean(filepath.Join(dataDir(e), "files"))
	p := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	return p, strings.HasPrefix(p, base+string(filepath.Separator))
}

func init() {
	ops.Register(ops.Op{Name: "attachment.list", Summary: "List a card's attachments", Method: "GET", Path: "/cards/{cardId}/attachments",
		Auth: ops.AuthPublic, UI: "GET /ui/cards/{id}/attachments"}, func(e *ops.Env, in CardIDIn) ([]AttachmentItem, error) {
		if _, err := authz.RequireCard(e, in.CardID, authz.View); err != nil {
			return nil, err
		}
		return query(e.Ctx, e.DB, scanAtt(dataDir(e)), attSelect+` WHERE a.card_id=? ORDER BY a.created_at,a.id`, in.CardID)
	})

	ops.Register(ops.Op{Name: "attachment.create", Summary: "Attach a file (multipart upload) or a link to a card", Method: "POST", MaxBody: 257 << 20, Path: "/cards/{cardId}/attachments",
		UI: "POST /ui/cards/{cardId}/attachments"}, func(e *ops.Env, in AttachmentCreateIn) (AttachmentItem, error) {
		board, err := authz.RequireCard(e, in.CardID, authz.Edit)
		if err != nil {
			return AttachmentItem{}, err
		}
		id := db.NewID()
		name := strings.TrimSpace(in.Name)
		if in.File == nil {
			u, perr := url.Parse(strings.TrimSpace(in.URL))
			if in.URL == "" || perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return AttachmentItem{}, ops.Invalid("provide a file upload or an http(s) url")
			}
			if name == "" {
				name = u.Host
			}
			_, err = e.DB.ExecContext(e.Ctx, `INSERT INTO attachments(id,card_id,creator_user_id,type,name,url,created_at) VALUES(?,?,?,'link',?,?,?)`,
				id, in.CardID, e.User.UserID, clip(name, 255), u.String(), db.Now())
			if err != nil {
				return AttachmentItem{}, err
			}
		} else {
			if len(in.File.Data) > maxUpload {
				return AttachmentItem{}, ops.Invalid("file too large")
			}
			if err := checkUserQuota(e, int64(len(in.File.Data))); err != nil {
				return AttachmentItem{}, err
			}
			fname := safeName(in.File.Name)
			if name == "" {
				name = fname
			}
			mime := http.DetectContentType(in.File.Data)
			if in.File.ContentType != "" && in.File.ContentType != "application/octet-stream" && mime == "application/octet-stream" {
				mime = in.File.ContentType
			}
			if i := strings.Index(mime, ";"); i > 0 {
				mime = mime[:i]
			}
			thumb, w, h, isImg := makeThumb(in.File.Data)
			rel := "attachments/" + id + "/" + fname
			dir := filepath.Join(dataDir(e), "files", "attachments", id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return AttachmentItem{}, err
			}
			if err := os.WriteFile(filepath.Join(dir, fname), in.File.Data, 0o644); err != nil {
				return AttachmentItem{}, err
			}
			stored := int64(len(in.File.Data))
			if thumb != nil {
				if err := os.WriteFile(filepath.Join(dir, "thumb.jpg"), thumb, 0o644); err == nil {
					stored += int64(len(thumb))
				}
			}
			var wv, hv any
			if isImg {
				wv, hv = w, h
			}
			err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(e.Ctx, `INSERT INTO attachments(id,card_id,creator_user_id,type,name,file,mime,size,is_image,width,height,created_at) VALUES(?,?,?,'file',?,?,?,?,?,?,?,?)`,
					id, in.CardID, e.User.UserID, clip(name, 255), rel, mime, len(in.File.Data), b2i(isImg), wv, hv, db.Now()); err != nil {
					return err
				}
				if isImg { // first image becomes the cover when the card has none
					if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET cover_attachment_id=? WHERE id=? AND cover_attachment_id IS NULL`, id, in.CardID); err != nil {
						return err
					}
				}
				return addStorage(tx, e, stored)
			})
			if err != nil {
				_ = os.RemoveAll(dir)
				return AttachmentItem{}, err
			}
		}
		emit(e, "attachmentCreate", board, in.CardID, "attachment.created", map[string]any{"attachmentId": id, "name": clip(name, 255)})
		return getAttachment(e, id)
	})

	attRef := func(e *ops.Env, id string, min authz.Level) (cardID, board, file, name string, err error) {
		var f sql.NullString
		if e.DB.QueryRowContext(e.Ctx, `SELECT a.card_id,c.board_id,a.file,a.name FROM attachments a JOIN cards c ON c.id=a.card_id WHERE a.id=?`, id).Scan(&cardID, &board, &f, &name) != nil {
			return "", "", "", "", ops.NotFound("attachment")
		}
		return cardID, board, f.String, name, authz.RequireBoard(e, board, min)
	}

	ops.Register(ops.Op{Name: "attachment.update", Summary: "Rename an attachment", Method: "PATCH", Path: "/attachments/{id}",
		UI: "POST /ui/attachments/{id}"}, func(e *ops.Env, in AttachmentUpdateIn) (AttachmentItem, error) {
		cardID, board, _, _, err := attRef(e, in.ID, authz.Edit)
		if err != nil {
			return AttachmentItem{}, err
		}
		n := strings.TrimSpace(in.Name)
		if n == "" {
			return AttachmentItem{}, ops.Invalid("name is required")
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE attachments SET name=? WHERE id=?`, clip(n, 255), in.ID); err != nil {
			return AttachmentItem{}, err
		}
		emit(e, "attachmentUpdate", board, cardID, "attachment.updated", map[string]any{"attachmentId": in.ID, "name": clip(n, 255)})
		return getAttachment(e, in.ID)
	})

	ops.Register(ops.Op{Name: "attachment.cover", Summary: "Set (or unset) an image attachment as the card cover", Method: "POST", Path: "/attachments/{id}/cover",
		UI: "POST /ui/attachments/{id}/cover"}, func(e *ops.Env, in AttachmentCoverIn) (AttachmentItem, error) {
		cardID, board, _, _, err := attRef(e, in.ID, authz.Edit)
		if err != nil {
			return AttachmentItem{}, err
		}
		if in.Unset {
			_, err = e.DB.ExecContext(e.Ctx, `UPDATE cards SET cover_attachment_id=NULL WHERE id=? AND cover_attachment_id=?`, cardID, in.ID)
		} else {
			var img int
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT is_image FROM attachments WHERE id=?`, in.ID).Scan(&img)
			if img == 0 {
				return AttachmentItem{}, ops.Invalid("only image attachments can be a cover")
			}
			_, err = e.DB.ExecContext(e.Ctx, `UPDATE cards SET cover_attachment_id=? WHERE id=?`, in.ID, cardID)
		}
		if err != nil {
			return AttachmentItem{}, err
		}
		emit(e, "attachmentCover", board, cardID, "card.updated", map[string]any{"attachmentId": in.ID, "unset": in.Unset})
		return getAttachment(e, in.ID)
	})

	ops.Register(ops.Op{Name: "attachment.delete", Summary: "Delete an attachment and its stored files", Method: "DELETE", Path: "/attachments/{id}",
		UI: "POST /ui/attachments/{id}/delete"}, func(e *ops.Env, in IDIn) (DeletedOut, error) {
		cardID, board, file, name, err := attRef(e, in.ID, authz.Edit)
		if err != nil {
			return DeletedOut{}, err
		}
		freed := removeAttachmentFiles(e, file)
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `UPDATE cards SET cover_attachment_id=NULL WHERE id=? AND cover_attachment_id=?`, cardID, in.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM attachments WHERE id=?`, in.ID); err != nil {
				return err
			}
			if freed > 0 {
				return addStorage(tx, e, -freed)
			}
			return nil
		})
		if err != nil {
			return DeletedOut{}, err
		}
		emit(e, "attachmentDelete", board, cardID, "attachment.deleted", map[string]any{"attachmentId": in.ID, "name": name})
		return DeletedOut{in.ID, cardID}, nil
	})

	serve := func(e *ops.Env, id string, thumb bool) (*ops.Stream, error) {
		var cardID, file, name, mime sql.NullString
		var board string
		if e.DB.QueryRowContext(e.Ctx, `SELECT a.card_id,c.board_id,a.file,a.name,a.mime FROM attachments a JOIN cards c ON c.id=a.card_id WHERE a.id=? AND a.type='file'`, id).
			Scan(&cardID, &board, &file, &name, &mime) != nil {
			return nil, ops.NotFound("attachment")
		}
		if err := authz.RequireBoard(e, board, authz.View); err != nil {
			return nil, ops.NotFound("attachment")
		}
		rel, ct := file.String, mime.String
		if thumb {
			rel, ct = path.Join(path.Dir(rel), "thumb.jpg"), "image/jpeg"
		}
		p, ok := attachmentPath(e, rel)
		if !ok {
			return nil, ops.NotFound("attachment")
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, ops.NotFound("attachment file")
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		n := name.String
		if thumb {
			n = "thumbnail.jpg"
		}
		return &ops.Stream{Name: n, ContentType: ct, Data: data}, nil
	}

	ops.Register(ops.Op{Name: "attachment.download", Summary: "Download an attachment file (authorized by board access)", Method: "GET", Path: "/attachments/{id}/download/{name}",
		Auth: ops.AuthPublic, Binary: true, UI: "GET /attachments/{id}/download/{name}"}, func(e *ops.Env, in AttachmentDownloadIn) (*ops.Stream, error) {
		return serve(e, in.ID, false)
	})
	ops.Register(ops.Op{Name: "attachment.thumbnail", Summary: "Download an image attachment's thumbnail", Method: "GET", Path: "/attachments/{id}/thumbnail",
		Auth: ops.AuthPublic, Binary: true, UI: "GET /attachments/{id}/thumbnail"}, func(e *ops.Env, in ThumbnailIn) (*ops.Stream, error) {
		return serve(e, in.ID, true)
	})

	ops.Register(ops.Op{Name: "attachment.media", Summary: "Board media view: all attachments of a board, filterable by image|video|doc", Method: "GET", Path: "/boards/{boardId}/media",
		Auth: ops.AuthPublic, UI: "GET /boards/{boardId}/media"}, func(e *ops.Env, in MediaIn) ([]AttachmentItem, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		q := attSelect + ` WHERE c.board_id=?`
		switch in.Type {
		case "", "all":
		case "image":
			q += ` AND a.is_image=1`
		case "video":
			q += ` AND a.is_image=0 AND a.mime LIKE 'video/%'`
		case "doc":
			q += ` AND a.is_image=0 AND COALESCE(a.mime,'') NOT LIKE 'video/%'`
		default:
			return nil, ops.Invalid("type must be all, image, video or doc")
		}
		return query(e.Ctx, e.DB, scanAtt(dataDir(e)), q+` ORDER BY a.created_at DESC,a.id DESC LIMIT 1000`, in.BoardID)
	})
}
