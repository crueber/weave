package boards

import (
	"bytes"
	"database/sql"
	"errors"
	"html/template"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register webp decoder

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/imgguard"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

const (
	maxBoardBackgroundBytes = 20 << 20
	boardBgThumbWidth       = 480
)

// BackgroundPatternInfo describes one selectable flat board background pattern.
// Every pattern is a seamless flat SVG tile under internal/web/static/patterns/<Slug>.svg (no gradients).
type BackgroundPatternInfo struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

// BackgroundPatterns is the curated set of 13 selectable board background patterns.
var BackgroundPatterns = []BackgroundPatternInfo{
	{Slug: "fill", Label: "Fill"},
	{Slug: "dots", Label: "Dots"},
	{Slug: "grid", Label: "Grid"},
	{Slug: "diagonal", Label: "Diagonal"},
	{Slug: "crosshatch", Label: "Crosshatch"},
	{Slug: "diamonds", Label: "Diamonds"},
	{Slug: "hexagons", Label: "Honeycomb"},
	{Slug: "waves", Label: "Waves"},
	{Slug: "topography", Label: "Topography"},
	{Slug: "herringbone", Label: "Herringbone"},
	{Slug: "isometric", Label: "Isometric"},
	{Slug: "arches", Label: "Arches"},
	{Slug: "terrazzo", Label: "Terrazzo"},
}

var validPatternSet = func() map[string]bool {
	m := make(map[string]bool, len(BackgroundPatterns))
	for _, p := range BackgroundPatterns {
		m[p.Slug] = true
	}
	return m
}()

// ValidPattern reports whether s is a supported board background pattern slug.
func ValidPattern(s string) bool { return validPatternSet[s] }

// NormalizePattern validates a board background pattern slug: "" clears it; unknown slugs return ops.Invalid.
func NormalizePattern(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "", nil
	}
	if validPatternSet[s] {
		return s, nil
	}
	names := make([]string, len(BackgroundPatterns))
	for i, p := range BackgroundPatterns {
		names[i] = p.Slug
	}
	return "", ops.Invalid("invalid backgroundPattern: use " + strings.Join(names, ", ") + " or empty to clear")
}

// BackgroundImage is an uploaded board background image.
type BackgroundImage struct {
	ID        string `json:"id"`
	BoardID   string `json:"boardId"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt string `json:"createdAt"`
}

type BoardBackgroundUploadIn struct {
	ID    string    `path:"id" desc:"Board id"`
	File  *ops.File `json:"file" required:"true" desc:"Image file (png, jpeg, gif, webp; max 20MB)"`
	Apply bool      `json:"apply" desc:"Also set it as the board's background image"`
}

type BoardBackgroundIn struct {
	ID      string `path:"id" desc:"Board id"`
	ImageID string `path:"imageId" desc:"Background image id"`
}

type BoardBackgroundFileIn struct {
	ID      string `path:"id" desc:"Board id"`
	ImageID string `path:"imageId" desc:"Background image id"`
	Variant string `path:"variant" desc:"original | thumbnail"`
}

func filesDir(e *ops.Env) (string, error) {
	switch c := e.Svc["config"].(type) {
	case config.Config:
		return filepath.Join(c.DataDir, "files"), nil
	case *config.Config:
		if c != nil {
			return filepath.Join(c.DataDir, "files"), nil
		}
	}
	return "", errors.New("storage is not configured")
}

func boardBgDir(e *ops.Env, boardID string) (string, error) {
	d, err := filesDir(e)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "board-backgrounds", boardID), nil
}

func thumbOf(file string) string { return file + ".thumb.jpg" }

func boardBgDims(e *ops.Env, file string) (int, int) {
	d, err := filesDir(e)
	if err != nil {
		return 0, 0
	}
	f, err := os.Open(filepath.Join(d, file))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// collectBoardBackgroundFiles collects disk file paths for boards about to be deleted.
func collectBoardBackgroundFiles(e *ops.Env, q querier, boardIDs []string) []string {
	if len(boardIDs) == 0 {
		return nil
	}
	root, err := filesDir(e)
	if err != nil {
		return nil
	}
	in := "(?" + strings.Repeat(",?", len(boardIDs)-1) + ")"
	args := make([]any, len(boardIDs))
	for i, id := range boardIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(e.Ctx, `SELECT file FROM board_background_images WHERE board_id IN `+in, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil && f != "" {
			out = append(out, filepath.Join(root, f), filepath.Join(root, thumbOf(f)))
		}
	}
	return out
}

func removeBoardBackgroundFiles(files []string) {
	dirs := map[string]bool{}
	for _, f := range files {
		_ = os.Remove(f)
		dirs[filepath.Dir(f)] = true
	}
	for d := range dirs {
		_ = os.Remove(d) // succeeds only when empty
	}
}

// ListBoardBackgrounds returns the uploaded background images of a board.
func ListBoardBackgrounds(e *ops.Env, boardID string) ([]BackgroundImage, error) {
	if err := authz.RequireBoard(e, boardID, authz.View); err != nil {
		return nil, err
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,board_id,COALESCE(size,0),created_at,file FROM board_background_images WHERE board_id=? ORDER BY created_at,id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackgroundImage{}
	for rows.Next() {
		var b BackgroundImage
		var file string
		if err := rows.Scan(&b.ID, &b.BoardID, &b.Size, &b.CreatedAt, &file); err != nil {
			return nil, err
		}
		b.Width, b.Height = boardBgDims(e, file)
		out = append(out, b)
	}
	return out, rows.Err()
}

func readBoardBackgroundFile(e *ops.Env, boardID, imageID, variant string) (*ops.Stream, error) {
	var file string
	if e.DB.QueryRowContext(e.Ctx, `SELECT file FROM board_background_images WHERE id=? AND board_id=?`, imageID, boardID).Scan(&file) != nil {
		return nil, ops.NotFound("background image")
	}
	root, err := filesDir(e)
	if err != nil {
		return nil, err
	}
	name, ct := file, map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[filepath.Ext(file)]
	switch variant {
	case "original":
	case "thumbnail":
		name, ct = thumbOf(file), "image/jpeg"
	default:
		return nil, ops.Invalid("variant must be original or thumbnail")
	}
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return nil, ops.NotFound("background image file")
	}
	return &ops.Stream{Name: imageID + filepath.Ext(name), ContentType: ct, Data: data}, nil
}

func makeBoardBgThumb(src image.Image) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > boardBgThumbWidth {
		h = h * boardBgThumbWidth / w
		if h < 1 {
			h = 1
		}
		w = boardBgThumbWidth
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func registerBoardBackgroundOps() {
	ops.Register(ops.Op{Name: "board.background.list", Summary: "List uploaded background images of a board", Method: "GET",
		Path: "/boards/{id}/background-images", UI: "GET /ui/boards/{id}/settings/background"}, func(e *ops.Env, in BoardGetIn) ([]BackgroundImage, error) {
		return ListBoardBackgrounds(e, in.ID)
	})

	ops.Register(ops.Op{Name: "board.background.upload", Summary: "Upload a board background image (png, jpeg, gif, webp; thumbnail generated)", Method: "POST", MaxBody: 21 << 20,
		Path: "/boards/{id}/background-images", UI: "POST /ui/boards/{id}/background-images"}, func(e *ops.Env, in BoardBackgroundUploadIn) (BackgroundImage, error) {
		if err := authz.RequireBoard(e, in.ID, authz.Edit); err != nil {
			return BackgroundImage{}, err
		}
		if in.File == nil || len(in.File.Data) == 0 {
			return BackgroundImage{}, ops.Invalid("file is required")
		}
		if len(in.File.Data) > maxBoardBackgroundBytes {
			return BackgroundImage{}, ops.Invalid("image too large (max 20MB)")
		}
		img, format, err := imgguard.Decode(in.File.Data)
		if errors.Is(err, imgguard.ErrTooLarge) {
			return BackgroundImage{}, ops.Invalid(err.Error())
		}
		if err != nil {
			return BackgroundImage{}, ops.Invalid("file is not a supported image (png, jpeg, gif, webp)")
		}
		ext := map[string]string{"png": ".png", "jpeg": ".jpg", "gif": ".gif", "webp": ".webp"}[format]
		if ext == "" {
			return BackgroundImage{}, ops.Invalid("unsupported image format " + format)
		}
		dir, err := boardBgDir(e, in.ID)
		if err != nil {
			return BackgroundImage{}, err
		}
		id := db.NewID()
		rel := filepath.Join("board-backgrounds", in.ID, id+ext)
		root, _ := filesDir(e)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return BackgroundImage{}, err
		}
		thumb, err := makeBoardBgThumb(img)
		if err != nil {
			return BackgroundImage{}, err
		}
		if err := os.WriteFile(filepath.Join(root, rel), in.File.Data, 0o644); err != nil {
			return BackgroundImage{}, err
		}
		if err := os.WriteFile(filepath.Join(root, thumbOf(rel)), thumb, 0o644); err != nil {
			_ = os.Remove(filepath.Join(root, rel))
			return BackgroundImage{}, err
		}
		now := db.Now()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO board_background_images(id,board_id,file,size,created_at) VALUES(?,?,?,?,?)`,
			id, in.ID, rel, len(in.File.Data), now); err != nil {
			_ = os.Remove(filepath.Join(root, rel))
			_ = os.Remove(filepath.Join(root, thumbOf(rel)))
			return BackgroundImage{}, err
		}
		b := img.Bounds()
		out := BackgroundImage{ID: id, BoardID: in.ID, Size: int64(len(in.File.Data)), Width: b.Dx(), Height: b.Dy(), CreatedAt: now}
		emit(e, "backgroundImageCreate", "backgroundImage.created", in.ID, map[string]any{"id": id, "boardId": in.ID})
		if in.Apply {
			ii := id
			if _, err := boardUpdate(e, BoardUpdateIn{ID: in.ID, BackgroundImageID: &ii}); err != nil {
				return out, err
			}
		}
		return out, nil
	})

	ops.Register(ops.Op{Name: "board.background.delete", Summary: "Delete an uploaded board background image", Method: "DELETE",
		Path: "/boards/{id}/background-images/{imageId}", UI: "DELETE /ui/boards/{id}/background-images/{imageId}"}, func(e *ops.Env, in BoardBackgroundIn) (Deleted, error) {
		if err := authz.RequireBoard(e, in.ID, authz.Edit); err != nil {
			return Deleted{}, err
		}
		var file string
		var cleared bool
		err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(e.Ctx, `SELECT file FROM board_background_images WHERE id=? AND board_id=?`, in.ImageID, in.ID).Scan(&file); err != nil {
				if db.IsNoRows(err) {
					return ops.NotFound("background image")
				}
				return err
			}
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM board_background_images WHERE id=?`, in.ImageID); err != nil {
				return err
			}
			res, err := tx.ExecContext(e.Ctx, `UPDATE boards SET background_image_id=NULL,updated_at=? WHERE id=? AND background_image_id=?`, db.Now(), in.ID, in.ImageID)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			cleared = n > 0
			return nil
		})
		if err != nil {
			return Deleted{}, err
		}
		if root, err := filesDir(e); err == nil {
			_ = os.Remove(filepath.Join(root, file))
			_ = os.Remove(filepath.Join(root, thumbOf(file)))
			_ = os.Remove(filepath.Dir(filepath.Join(root, file)))
		}
		emit(e, "backgroundImageDelete", "backgroundImage.deleted", in.ID, map[string]any{"id": in.ImageID})
		if cleared {
			emit(e, "boardUpdate", "board.updated", in.ID, map[string]any{"id": in.ID})
		}
		return Deleted{in.ID}, nil
	})

	ops.Register(ops.Op{Name: "board.background.file", Summary: "Download a board background image (variant: original|thumbnail)", Method: "GET",
		Path: "/boards/{id}/background-images/{imageId}/{variant}", UI: "GET /boards/{id}/background-images/{imageId}/{variant}", Binary: true},
		func(e *ops.Env, in BoardBackgroundFileIn) (*ops.Stream, error) {
			if err := authz.RequireBoard(e, in.ID, authz.View); err != nil {
				return nil, err
			}
			return readBoardBackgroundFile(e, in.ID, in.ImageID, in.Variant)
		})
}

func init() {
	web.AddFuncs(template.FuncMap{
		"boardsPatterns": func() []BackgroundPatternInfo { return BackgroundPatterns },
		"boardsBgImageURL": func(b Board, pageURL string) string {
			if b.BackgroundImageID == nil || *b.BackgroundImageID == "" {
				return ""
			}
			if strings.HasPrefix(pageURL, "/public/") {
				return pageURL + "/background-image?v=" + *b.BackgroundImageID
			}
			return "/boards/" + b.ID + "/background-images/" + *b.BackgroundImageID + "/original"
		},
	})
}
