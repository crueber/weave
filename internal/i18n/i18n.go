// Package i18n provides translation lookup. Locale files live in locales/<lang>.json
// as flat key->string maps; English is the fallback, then the key itself.
package i18n

import (
	"embed"
	"encoding/json"
	"html/template"
	"strings"
	"sync"

	"github.com/crueber/weave/internal/web"
)

//go:embed locales/*.json
var fsys embed.FS

var (
	once sync.Once
	tabs map[string]map[string]string
)

func load() {
	tabs = map[string]map[string]string{}
	ents, _ := fsys.ReadDir("locales")
	for _, e := range ents {
		b, err := fsys.ReadFile("locales/" + e.Name())
		if err != nil {
			continue
		}
		m := map[string]string{}
		if json.Unmarshal(b, &m) == nil {
			tabs[strings.TrimSuffix(e.Name(), ".json")] = m
		}
	}
}

// T returns the translation of key for lang (e.g. "de", "pt-BR"), falling back
// to the base language, then English, then the key itself.
func T(lang, key string) string {
	once.Do(load)
	base, _, _ := strings.Cut(lang, "-")
	for _, l := range []string{lang, base, "en"} {
		if v, ok := tabs[l][key]; ok && l != "" {
			return v
		}
	}
	return key
}

// Languages lists the available language codes.
func Languages() []string {
	once.Do(load)
	var out []string
	for k := range tabs {
		out = append(out, k)
	}
	return out
}

func init() {
	// Templates: {{t "en" "common.save"}} (pass the user's language as first arg).
	web.AddFuncs(template.FuncMap{"t": T})
}
