package auth

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/crueber/weave/internal/imgguard"
	"github.com/crueber/weave/internal/ops"
)

const (
	maxAvatarBytes = 5 << 20
	avatarSize     = 256
)

// normalizeAvatar decodes an uploaded image (png/jpeg/gif/webp), center-crops it
// to a square, scales it to 256px and re-encodes it as PNG (strips metadata).
func normalizeAvatar(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty file")
	}
	if len(data) > maxAvatarBytes {
		return nil, errors.New("avatar is larger than 5 MB")
	}
	img, _, err := imgguard.Decode(data)
	if errors.Is(err, imgguard.ErrTooLarge) {
		return nil, err
	}
	if err != nil {
		return nil, errors.New("avatar must be a png, jpeg, gif or webp image")
	}
	b := img.Bounds()
	side := min(b.Dx(), b.Dy())
	if side == 0 {
		return nil, errors.New("invalid image")
	}
	src := image.Rect(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2, 0, 0)
	src.Max = src.Min.Add(image.Pt(side, side))
	out := image.NewRGBA(image.Rect(0, 0, avatarSize, avatarSize))
	draw.CatmullRom.Scale(out, out.Bounds(), img, src, draw.Over, nil)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func removeAvatarFile(e *ops.Env, userID string) {
	var f string
	if e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(avatar,'') FROM users WHERE id=?`, userID).Scan(&f) == nil && f != "" {
		_ = os.Remove(filepath.Join(dataDir(e), "files", "avatars", filepath.Base(f)))
	}
}
