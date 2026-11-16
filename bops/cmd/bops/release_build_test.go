package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReleaseBuild runs scripts/release-build.sh, which the bops/v* release
// workflow runs on a tag (bops-ss6.14): every target is archived with the
// binary and the docs, checksums.txt matches the archives, the host's binary
// prints the stamped version, and a version without a leading v exits 2.
func TestReleaseBuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release-build.sh is a POSIX sh script")
	}
	script, _ := filepath.Abs("../../../scripts/release-build.sh")
	const v = "v0.0.0-test"
	dist := t.TempDir()
	if out, err := exec.Command("sh", script, v, dist).CombinedOutput(); err != nil {
		t.Fatalf("release-build.sh: %v\n%s", err, out)
	}

	targets := []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64", "windows_amd64", "windows_arm64", "android_arm64"}
	want := map[string]bool{}
	for _, tg := range targets {
		name := "bops_" + v + "_" + tg
		if strings.HasPrefix(tg, "windows") {
			want[name+".zip"] = true
			zr, err := zip.OpenReader(filepath.Join(dist, name+".zip"))
			if err != nil {
				t.Fatalf("%s: %v", tg, err)
			}
			var has bool
			for _, f := range zr.File {
				has = has || f.Name == name+"/bops.exe"
			}
			zr.Close()
			if !has {
				t.Errorf("%s: zip has no %s/bops.exe", tg, name)
			}
		} else {
			want[name+".tar.gz"] = true
		}
	}

	f, err := os.Open(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	seen := 0
	for sc.Scan() {
		sum, file, ok := strings.Cut(sc.Text(), "  ")
		if !ok || !want[file] {
			t.Errorf("checksums.txt: unexpected line %q", sc.Text())
			continue
		}
		b, err := os.ReadFile(filepath.Join(dist, file))
		if err != nil {
			t.Fatal(err)
		}
		if h := sha256.Sum256(b); hex.EncodeToString(h[:]) != sum {
			t.Errorf("checksums.txt: %s does not match", file)
		}
		seen++
	}
	if seen != len(targets) {
		t.Errorf("checksums.txt lists %d archives, want %d", seen, len(targets))
	}

	host := "bops_" + v + "_" + runtime.GOOS + "_" + runtime.GOARCH
	if !want[host+".tar.gz"] {
		t.Logf("no archive for the host %s/%s; skipping the version check", runtime.GOOS, runtime.GOARCH)
	} else {
		dir := t.TempDir()
		files := untar(t, filepath.Join(dist, host+".tar.gz"), dir)
		for _, n := range []string{"bops", "LICENSE", "README.md", "CHANGELOG.md"} {
			if !files[host+"/"+n] {
				t.Errorf("%s.tar.gz has no %s", host, n)
			}
		}
		t.Chdir(t.TempDir())
		out, err := exec.Command(filepath.Join(dir, host, "bops"), "version", "--json").Output()
		if err != nil {
			t.Fatalf("bops version: %v", err)
		}
		var doc struct{ Bops string }
		if err := json.Unmarshal(out, &doc); err != nil || doc.Bops != v {
			t.Errorf("bops version printed bops=%q (%v), want %q", doc.Bops, err, v)
		}
	}

	err = exec.Command("sh", script, "0.0.0", t.TempDir()).Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Errorf("a version without a leading v: got %v, want exit 2", err)
	}
}

// untar extracts a .tar.gz into dir and returns the names it held.
func untar(t *testing.T, path, dir string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[strings.TrimSuffix(h.Name, "/")] = true
		if h.Typeflag != tar.TypeReg {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, os.FileMode(h.Mode))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
		out.Close()
	}
}
