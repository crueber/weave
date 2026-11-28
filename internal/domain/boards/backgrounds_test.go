package boards_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func bgPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{0x3e, 0x63, 0xdd, 0xff})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestBoardBackgroundPatternsCatalogAndNoGradients(t *testing.T) {
	if len(boards.BackgroundPatterns) < 10 {
		t.Fatalf("want at least 10 patterns, got %d", len(boards.BackgroundPatterns))
	}
	seen := map[string]bool{}
	for _, p := range boards.BackgroundPatterns {
		if !boards.ValidPattern(p.Slug) {
			t.Fatalf("pattern %q not valid", p.Slug)
		}
		if seen[p.Slug] {
			t.Fatalf("duplicate pattern %q", p.Slug)
		}
		seen[p.Slug] = true
		data, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "patterns", p.Slug+".svg"))
		if err != nil {
			t.Fatalf("pattern %s svg missing: %v", p.Slug, err)
		}
		s := strings.ToLower(string(data))
		if !strings.Contains(s, "<svg") {
			t.Fatalf("pattern %s invalid svg", p.Slug)
		}
		if strings.Contains(s, "gradient") {
			t.Fatalf("pattern %s must not contain gradients", p.Slug)
		}
	}
	if _, err := boards.NormalizePattern("nonexistent"); err == nil {
		t.Fatal("unknown pattern must fail")
	}
	if got, err := boards.NormalizePattern("  Dots "); err != nil || got != "dots" {
		t.Fatalf("NormalizePattern: %q %v", got, err)
	}
	if got, err := boards.NormalizePattern(""); err != nil || got != "" {
		t.Fatalf("empty pattern: %q %v", got, err)
	}
}

func TestBoardBackgroundOpsUIAndCleanup(t *testing.T) {
	e := fixture(t)
	dataDir := t.TempDir()
	e.Svc["config"] = config.Config{DataDir: dataDir}
	vw := testutil.As(e, viewer)
	stranger := testutil.As(e, nobody)
	b := newBoard(t, e, "BG Board")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})

	// Set colour + pattern
	col, pat := "#3e63dd", "topography"
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, Color: &col, BackgroundPattern: &pat})
	if b.Color == nil || *b.Color != "#3e63dd" || b.BackgroundPattern == nil || *b.BackgroundPattern != "topography" {
		t.Fatalf("expected colour+pattern, got %+v", b)
	}
	// Invalid pattern rejected
	bad := "lava-lamp"
	if _, err := ops.Call(e, "board.update", &boards.BoardUpdateIn{ID: b.ID, BackgroundPattern: &bad}); err == nil {
		t.Fatal("invalid pattern must fail")
	}

	// Viewer cannot upload; stranger gets not_found
	if _, err := ops.Call(vw, "board.background.upload", &boards.BoardBackgroundUploadIn{ID: b.ID, File: &ops.File{Name: "a.png", Data: bgPNG(64, 48)}, Apply: true}); err == nil || err.(*ops.Error).Status != 403 {
		t.Fatalf("viewer upload: %v", err)
	}
	if _, err := ops.Call(stranger, "board.background.upload", &boards.BoardBackgroundUploadIn{ID: b.ID, File: &ops.File{Name: "a.png", Data: bgPNG(64, 48)}, Apply: true}); err == nil || err.(*ops.Error).Status != 404 {
		t.Fatalf("stranger upload: %v", err)
	}

	// Editor uploads and applies image -> clears pattern, sets image
	img := call[boards.BackgroundImage](t, e, "board.background.upload", boards.BoardBackgroundUploadIn{
		ID: b.ID, File: &ops.File{Name: "art.png", Data: bgPNG(80, 60)}, Apply: true,
	})
	if img.Width != 80 || img.Height != 60 || img.BoardID != b.ID {
		t.Fatalf("uploaded img: %+v", img)
	}
	detail := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID})
	if detail.Board.BackgroundImageID == nil || *detail.Board.BackgroundImageID != img.ID || detail.Board.BackgroundPattern != nil {
		t.Fatalf("apply image should clear pattern: %+v", detail.Board)
	}

	// List & file download (original + thumbnail)
	list := call[[]boards.BackgroundImage](t, vw, "board.background.list", boards.BoardGetIn{ID: b.ID})
	if len(list) != 1 || list[0].ID != img.ID {
		t.Fatalf("list: %+v", list)
	}
	st := call[*ops.Stream](t, vw, "board.background.file", boards.BoardBackgroundFileIn{ID: b.ID, ImageID: img.ID, Variant: "original"})
	if st.ContentType != "image/png" || len(st.Data) == 0 {
		t.Fatalf("stream: %+v", st)
	}
	thumb := call[*ops.Stream](t, vw, "board.background.file", boards.BoardBackgroundFileIn{ID: b.ID, ImageID: img.ID, Variant: "thumbnail"})
	if thumb.ContentType != "image/jpeg" || len(thumb.Data) == 0 {
		t.Fatalf("thumb: %+v", thumb)
	}

	// Board page renders bottom-left background image; public board serves its active image
	pub := call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true})
	m := mux(e)
	code, body := get(t, m, "/boards/"+b.ID, false)
	if code != 200 || !strings.Contains(body, `data-board-bg="image"`) || !strings.Contains(body, "/boards/"+b.ID+"/background-images/"+img.ID+"/original") {
		t.Fatalf("board page missing bg image (%d)", code)
	}
	code, body = get(t, m, "/boards/"+b.ID+"/settings", false)
	if code != 200 || !strings.Contains(body, "Background pattern") || !strings.Contains(body, `data-cap="board-background-upload"`) {
		t.Fatalf("settings page missing bg controls (%d)", code)
	}
	anonEnv := testutil.As(e, nil)
	am := mux(anonEnv)
	code, body = get(t, am, "/public/"+pub.PublicToken, false)
	if code != 200 || !strings.Contains(body, "/public/"+pub.PublicToken+"/background-image?v="+img.ID) {
		t.Fatalf("public page missing bg image (%d)", code)
	}
	code, _ = get(t, am, "/public/"+pub.PublicToken+"/background-image", false)
	if code != 200 {
		t.Fatalf("public bg image fetch: %d", code)
	}

	// Selecting a pattern clears the image; export/import preserves backgroundPattern
	pat2 := "hexagons"
	b = call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, BackgroundPattern: &pat2})
	if b.BackgroundPattern == nil || *b.BackgroundPattern != "hexagons" || b.BackgroundImageID != nil {
		t.Fatalf("switching to pattern: %+v", b)
	}
	exp := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID, Format: "json"})
	if !strings.Contains(string(exp.Data), `"backgroundPattern": "hexagons"`) {
		t.Fatalf("export missing backgroundPattern: %s", exp.Data)
	}
	imp := call[boards.Board](t, e, "board.import", boards.ImportIn{ProjectID: "p1", Format: "planka", Data: string(exp.Data), Name: "Imported BG"})
	if imp.BackgroundPattern == nil || *imp.BackgroundPattern != "hexagons" {
		t.Fatalf("import lost backgroundPattern: %+v", imp)
	}

	// Deleting the board removes uploaded background image files from disk
	bgDir := filepath.Join(dataDir, "files", "board-backgrounds", b.ID)
	entries, err := os.ReadDir(bgDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected files in %s before delete, got %v %v", bgDir, entries, err)
	}
	call[boards.Deleted](t, e, "board.delete", boards.BoardGetIn{ID: b.ID})
	if _, err := os.Stat(bgDir); !os.IsNotExist(err) {
		t.Fatalf("expected %s cleaned up after board delete, stat err=%v", bgDir, err)
	}
}
