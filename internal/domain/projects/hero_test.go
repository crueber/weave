package projects

import (
	"os"
	"strings"
	"testing"
)

// Flat colours and gradients are never painted as a full-saturation block: HeroCSS / CoverCSS only publish
// --hero-color and the stylesheet draws a faint tint plus a 4px band; images keep their background.
func TestFlatBackgroundsAreTinted(t *testing.T) {
	for _, p := range []Project{{BackgroundType: "color", BackgroundColor: "#dc2626"}, {BackgroundType: "gradient", BackgroundGradient: "sun-scream"}} {
		for name, got := range map[string]string{"hero": string(HeroCSS(p)), "cover": string(CoverCSS(p))} {
			if !strings.HasPrefix(got, "--hero-color:#") || strings.Contains(got, "background") {
				t.Errorf("%s for %s: %q", name, p.BackgroundType, got)
			}
		}
	}
	img := Project{ID: "abc123", BackgroundType: "image", BackgroundImageID: "def456"}
	if !strings.Contains(string(CoverCSS(img)), "thumbnail") {
		t.Error("image tile cover keeps the thumbnail")
	}
	b, err := os.ReadFile("../../web/static/projects_ui.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{"height:14rem", "color-mix(in oklab,var(--hc) 16%,var(--surface))", "height:4px;background:var(--hc)", "max-width:min(32.5rem", "-webkit-line-clamp:2", "projects-hero-tools", "color-mix(in oklab,var(--bg) 28%,transparent)"} {
		if !strings.Contains(css, want) {
			t.Errorf("projects_ui.css missing %q", want)
		}
	}
	if strings.Contains(css, "gradient(") {
		t.Error("no gradients")
	}
}

func TestHeroCSS(t *testing.T) {
	img := Project{ID: "abc123", BackgroundType: "image", BackgroundImageID: "def456"}
	css := string(HeroCSS(img))
	if !strings.Contains(css, "center/cover") || !strings.Contains(css, "/original)") || strings.Contains(css, "thumbnail") {
		t.Errorf("hero image must be a full-size cover background, got %q", css)
	}
	// tiles on the home page keep the thumbnail
	if !strings.Contains(string(BackgroundCSS(img)), "thumbnail") {
		t.Error("tile cover keeps the thumbnail")
	}
	// hostile ids and colours never reach the style
	if HeroCSS(Project{ID: "abc", BackgroundType: "image", BackgroundImageID: "x);evil:(1"}) != "" {
		t.Error("unvalidated image id must render nothing")
	}
	if HeroCSS(Project{BackgroundType: "color", BackgroundColor: "red;x:y"}) != "" {
		t.Error("unvalidated colour must render nothing")
	}
	if got := string(HeroCSS(Project{BackgroundType: "color", BackgroundColor: "#3e63dd"})); got != "--hero-color:#3e63dd" {
		t.Errorf("flat colour: %q", got)
	}
}
