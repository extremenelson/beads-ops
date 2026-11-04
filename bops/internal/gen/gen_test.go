package gen_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	bops "github.com/extremenelson/beads-ops/bops"
	"github.com/extremenelson/beads-ops/bops/internal/gen"
	"github.com/extremenelson/beads-ops/bops/internal/manifest"
)

var update = flag.Bool("update", false, "rewrite golden files (a diff here is a contract review)")

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
