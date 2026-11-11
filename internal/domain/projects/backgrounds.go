package projects

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register webp decoder

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/imgguard"
	"github.com/crueber/weave/internal/ops"
)

const (
	maxBackgroundBytes = 20 << 20
	thumbWidth         = 480
)

// BackgroundImage is an uploaded project background.
type BackgroundImage struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt string `json:"createdAt"`
}

type BackgroundUploadIn struct {
	ID    string    `path:"id" desc:"Project id"`
	File  *ops.File `json:"file" required:"true" desc:"Image file (png, jpeg, gif, webp; max 20MB)"`
	Apply bool      `json:"apply" desc:"Also set it as the project's background"`
}

type BackgroundIn struct {
	ID      string `path:"id" desc:"Project id"`
	ImageID string `path:"imageId" desc:"Background image id"`
}

type BackgroundFileIn struct {
	ID      string `path:"id" desc:"Project id"`
	ImageID string `path:"imageId" desc:"Background image id"`
	Variant string `path:"variant" desc:"original | thumbnail"`
}

// FilesDir returns $WEAVE_DATA_DIR/files (cfg is e.Svc["config"]).
func FilesDir(e *ops.Env) (string, error) {
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

func bgDir(e *ops.Env, projectID string) (string, error) {
	d, err := FilesDir(e)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "backgrounds", projectID), nil
}

func imageFiles(e *ops.Env, projectID string) []string {
	var out []string
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT file FROM background_images WHERE project_id=?`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	d, derr := FilesDir(e)
	if derr != nil {
		return nil
	}
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil {
			out = append(out, filepath.Join(d, f), filepath.Join(d, thumbOf(f)))
		}
	}
	return out
}

func thumbOf(file string) string { return file + ".thumb.jpg" }

func removeFiles(e *ops.Env, files []string) {
	for _, f := range files {
		_ = os.Remove(f)
	}
	if len(files) > 0 {
		_ = os.Remove(filepath.Dir(files[0])) // only succeeds when empty
	}
}

// dims reads image dimensions from the stored file (the table has no such columns).
func dims(e *ops.Env, file string) (int, int) {
	d, err := FilesDir(e)
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

// ListBackgrounds returns the uploaded images of a project.
func ListBackgrounds(e *ops.Env, projectID string) ([]BackgroundImage, error) {
	if err := authz.RequireProject(e, projectID, authz.View); err != nil {
		return nil, err
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,project_id,COALESCE(size,0),created_at,file FROM background_images WHERE project_id=? ORDER BY created_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackgroundImage{}
	for rows.Next() {
		var b BackgroundImage
		var file string
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.Size, &b.CreatedAt, &file); err != nil {
			return nil, err
		}
		b.Width, b.Height = dims(e, file)
		out = append(out, b)
	}
	return out, rows.Err()
}

func init() {
	ops.Register(ops.Op{Name: "project.background.list", Summary: "List uploaded background images", Method: "GET", Path: "/projects/{id}/background-images",
		UI: "GET /ui/projects/{id}/settings/background"}, func(e *ops.Env, in IDIn) ([]BackgroundImage, error) {
		return ListBackgrounds(e, in.ID)
	})

	ops.Register(ops.Op{Name: "project.background.upload", Summary: "Upload a background image (thumbnail generated)", Method: "POST", MaxBody: 21 << 20, Path: "/projects/{id}/background-images",
		UI: "POST /ui/projects/{id}/background-images"}, func(e *ops.Env, in BackgroundUploadIn) (BackgroundImage, error) {
		if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
			return BackgroundImage{}, err
		}
		if in.File == nil || len(in.File.Data) == 0 {
			return BackgroundImage{}, ops.Invalid("file is required")
		}
		if len(in.File.Data) > maxBackgroundBytes {
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
		dir, err := bgDir(e, in.ID)
		if err != nil {
			return BackgroundImage{}, err
		}
		id := db.NewID()
		rel := filepath.Join("backgrounds", in.ID, id+ext)
		root, _ := FilesDir(e)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return BackgroundImage{}, err
		}
		thumb, err := makeThumb(img)
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
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO background_images(id,project_id,file,size,created_at) VALUES(?,?,?,?,?)`,
			id, in.ID, rel, len(in.File.Data), now); err != nil {
			_ = os.Remove(filepath.Join(root, rel))
			_ = os.Remove(filepath.Join(root, thumbOf(rel)))
			return BackgroundImage{}, err
		}
		b := img.Bounds()
		out := BackgroundImage{ID: id, ProjectID: in.ID, Size: int64(len(in.File.Data)), Width: b.Dx(), Height: b.Dy(), CreatedAt: now}
		publish(e, "backgroundImage.created", in.ID, out)
		if in.Apply {
			t, ii := "image", id
			if _, err := updateProject(e, UpdateIn{ID: in.ID, BackgroundType: &t, BackgroundImageID: &ii}); err != nil {
				return out, err
			}
		}
		return out, nil
	})

	ops.Register(ops.Op{Name: "project.background.delete", Summary: "Delete an uploaded background image", Method: "DELETE",
		Path: "/projects/{id}/background-images/{imageId}", UI: "DELETE /ui/projects/{id}/background-images/{imageId}"},
		func(e *ops.Env, in BackgroundIn) (DeleteOut, error) {
			if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
				return DeleteOut{}, err
			}
			var file string
			if e.DB.QueryRowContext(e.Ctx, `SELECT file FROM background_images WHERE id=? AND project_id=?`, in.ImageID, in.ID).Scan(&file) != nil {
				return DeleteOut{}, ops.NotFound("background image")
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM background_images WHERE id=?`, in.ImageID); err != nil {
				return DeleteOut{}, err
			}
			_, _ = e.DB.ExecContext(e.Ctx, `UPDATE projects SET background_image_id=NULL,background_type=NULL,updated_at=? WHERE id=? AND background_image_id=?`, db.Now(), in.ID, in.ImageID)
			if root, err := FilesDir(e); err == nil {
				_ = os.Remove(filepath.Join(root, file))
				_ = os.Remove(filepath.Join(root, thumbOf(file)))
			}
			publish(e, "backgroundImage.deleted", in.ID, map[string]string{"id": in.ImageID})
			if p, err := Get(e, in.ID); err == nil {
				publish(e, "project.updated", in.ID, p)
			}
			return DeleteOut{in.ImageID}, nil
		})

	ops.Register(ops.Op{Name: "project.background.file", Summary: "Download a background image (variant: original|thumbnail)", Method: "GET",
		Path: "/projects/{id}/background-images/{imageId}/{variant}", UI: "GET /projects/{id}/background-images/{imageId}/{variant}", Binary: true},
		func(e *ops.Env, in BackgroundFileIn) (*ops.Stream, error) {
			if err := authz.RequireProject(e, in.ID, authz.View); err != nil {
				return nil, err
			}
			var file string
			if e.DB.QueryRowContext(e.Ctx, `SELECT file FROM background_images WHERE id=? AND project_id=?`, in.ImageID, in.ID).Scan(&file) != nil {
				return nil, ops.NotFound("background image")
			}
			root, err := FilesDir(e)
			if err != nil {
				return nil, err
			}
			name, ct := file, map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[filepath.Ext(file)]
			switch in.Variant {
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
			return &ops.Stream{Name: in.ImageID + filepath.Ext(name), ContentType: ct, Data: data}, nil
		})
}

func makeThumb(src image.Image) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbWidth {
		h = h * thumbWidth / w
		if h < 1 {
			h = 1
		}
		w = thumbWidth
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// flatten transparency onto white so JPEG output looks right
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
