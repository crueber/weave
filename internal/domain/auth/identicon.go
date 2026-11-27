package auth

import (
	"sync"

	dicebear "github.com/dicebear/dicebear-go/v10"
	"github.com/dicebear/styles/v10"
)

// Default avatars are generated in-process with DiceBear's "Landscape" style
// (CC0 1.0, by DiceBear): no network and no external API. The seed is the stable
// user id (never the email), so a person always gets the same picture.
const avatarCacheMax = 4096

var (
	landscapeOnce  sync.Once
	landscapeStyle *dicebear.Style
	landscapeErr   error
	avatarCacheMu  sync.Mutex
	avatarCache    = map[string][]byte{}
	avatarRing     = make([]string, 0, avatarCacheMax)
	avatarRingPos  int
)

// GeneratedAvatarSVG returns the deterministic Landscape avatar for seed.
func GeneratedAvatarSVG(seed string) ([]byte, error) {
	avatarCacheMu.Lock()
	if svg, ok := avatarCache[seed]; ok {
		avatarCacheMu.Unlock()
		return svg, nil
	}
	avatarCacheMu.Unlock()

	landscapeOnce.Do(func() { landscapeStyle, landscapeErr = dicebear.NewStyle([]byte(styles.Landscape)) })
	if landscapeErr != nil {
		return nil, landscapeErr
	}
	av, err := dicebear.NewAvatar(landscapeStyle, map[string]any{"seed": seed})
	if err != nil {
		return nil, err
	}
	svg := []byte(av.SVG())

	avatarCacheMu.Lock()
	if _, exists := avatarCache[seed]; !exists {
		if len(avatarRing) < avatarCacheMax {
			avatarRing = append(avatarRing, seed)
		} else {
			delete(avatarCache, avatarRing[avatarRingPos])
			avatarRing[avatarRingPos] = seed
			avatarRingPos = (avatarRingPos + 1) % avatarCacheMax
		}
		avatarCache[seed] = svg
	}
	avatarCacheMu.Unlock()
	return svg, nil
}
