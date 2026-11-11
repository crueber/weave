package auth

import (
	"sync"

	dicebear "github.com/dicebear/dicebear-go/v10"
	"github.com/dicebear/styles/v10"
)

// Default avatars are generated in-process with DiceBear's "Landscape" style
// (CC0 1.0, by DiceBear): no network and no external API. The seed is the stable
// user id (never the email), so a person always gets the same picture.
var (
	landscapeOnce  sync.Once
	landscapeStyle *dicebear.Style
	landscapeErr   error
	avatarCache    sync.Map // seed -> []byte (SVGs are small and deterministic)
)

// GeneratedAvatarSVG returns the deterministic Landscape avatar for seed.
func GeneratedAvatarSVG(seed string) ([]byte, error) {
	if v, ok := avatarCache.Load(seed); ok {
		return v.([]byte), nil
	}
	landscapeOnce.Do(func() { landscapeStyle, landscapeErr = dicebear.NewStyle([]byte(styles.Landscape)) })
	if landscapeErr != nil {
		return nil, landscapeErr
	}
	av, err := dicebear.NewAvatar(landscapeStyle, map[string]any{"seed": seed})
	if err != nil {
		return nil, err
	}
	svg := []byte(av.SVG())
	// bound the cache: ids are server-chosen, but keep memory predictable
	n := 0
	avatarCache.Range(func(k, _ any) bool { n++; return n < 4096 })
	if n < 4096 {
		avatarCache.Store(seed, svg)
	}
	return svg, nil
}
