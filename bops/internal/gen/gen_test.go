package gen_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

var update = flag.Bool("update", false, "rewrite golden files (a diff here is a contract review)")

// TestDocsArgsAsTheCommandsTakeThem: the docs render each arg the way the
// command line takes it (manifest.Arg.Flag): the id positional, stdin|file
// text as --file, never --id or --text.
func TestDocsArgsAsTheCommandsTakeThem(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	out, err := gen.Docs(m)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	for _, bad := range []string{"| `--id` |", "| `--text` |", "| stdin|file"} {
		if strings.Contains(doc, bad) {
			t.Errorf("docs contain %q", bad)
		}
	}
	section := func(name string) string {
		s := doc[strings.Index(doc, "## `bops "+name+"`"):]
		if i := strings.Index(s[1:], "\n## "); i >= 0 {
			s = s[:i+1]
		}
		return s
	}
	for _, c := range []struct{ cmd, want string }{
		{"claim", "| `<id>` (positional) | string |"},
		{"note", "| `--file <path>` |"},
		{"release", "| `--file <path>` \\| `--file -` |"},
		{"release", "| `--reason` | enum:"},
	} {
		if !strings.Contains(section(c.cmd), c.want) {
			t.Errorf("bops %s: docs lack %q", c.cmd, c.want)
		}
	}
	if strings.Contains(section("release"), "| `--file <path>` |") {
		t.Errorf("bops release: its optional text also takes --file -")
	}
}

func TestGolden(t *testing.T) {
	m, err := manifest.Parse(bops.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range gen.Targets() {
		t.Run(target, func(t *testing.T) {
			g, _ := gen.Lookup(target)
			got, err := g(m)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("..", "..", "testdata", "golden", target+".golden")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: generated output differs from golden; if intended, re-run with -update and review the diff as a contract change", target)
			}
		})
	}
}
