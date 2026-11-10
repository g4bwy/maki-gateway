package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uiDirWithIndex(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveWebDirUsesFlagPath(t *testing.T) {
	want := uiDirWithIndex(t, filepath.Join(t.TempDir(), "ui"))
	got, err := resolveWebDir(want, filepath.Join(t.TempDir(), "bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
}

func TestResolveWebDirFallsBackBesideExecutable(t *testing.T) {
	root := t.TempDir()
	want := uiDirWithIndex(t, filepath.Join(root, "bin", "web"))
	got, err := resolveWebDir("no-such-ui", filepath.Join(root, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("dir = %q, want %q", got, want)
	}
}

func TestResolveWebDirRefusesMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	empty := t.TempDir()
	_, err := resolveWebDir(missing, empty)
	if err == nil {
		t.Fatal("resolveWebDir must refuse when no candidate holds index.html")
	}
	for _, want := range []string{missing, filepath.Join(empty, "web"), "index.html", "-web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err, want)
		}
	}
}

func TestResolveWebDirRefusesDirWithoutIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("// ui"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWebDir(dir, ""); err == nil {
		t.Fatal("resolveWebDir must refuse a directory with no index.html")
	}
	if _, err := resolveWebDir(dir, "x"); err == nil {
		t.Fatal("no executable dir means no fallback")
	}
}

func TestResolveWebDirAcceptsRelativePathFromCwd(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	want := uiDirWithIndex(t, filepath.Join(root, "web"))
	t.Chdir(root)
	got, err := resolveWebDir("web", "")
	if err != nil {
		t.Fatalf("a relative -web must resolve from the cwd: %v", err)
	}
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("dir = %q, want absolute %q", got, want)
	}
}
