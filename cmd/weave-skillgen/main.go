// Command weave-skillgen renders skills/weave/** and internal/cli/snapshot.json from the op
// registry plus the hand-written sources in internal/skillgen/src. Run it with `make skill`
// (or `go generate ./...`) after adding or changing any op.
//
//	go run ./cmd/weave-skillgen [-root DIR] [-check]
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/crueber/weave/internal/cli/opsbridge"
	"github.com/crueber/weave/internal/skillgen"

	_ "github.com/crueber/weave/internal/all"
)

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "do not write; exit 1 if anything is stale")
	flag.Parse()
	files := skillgen.Render(opsbridge.FromRegistry())
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	stale := 0
	for _, n := range names {
		p := filepath.Join(*root, filepath.FromSlash(n))
		cur, _ := os.ReadFile(p)
		if bytes.Equal(cur, files[n]) {
			continue
		}
		stale++
		if *check {
			fmt.Fprintln(os.Stderr, "stale:", n)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(p, files[n], 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote", n)
	}
	if *check && stale > 0 {
		fmt.Fprintln(os.Stderr, skillgen.RegenHint())
		os.Exit(1)
	}
}
