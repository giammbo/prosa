package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadParsesFrontmatterAndDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "database.md", "# DB")
	write(t, dir, "cron.md", "---\nautonomy: auto\ndepends_on: [database]\n---\n# X")
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "cron" || got[1].Name != "database" {
		t.Fatalf("unexpected result: %+v", got)
	}
	c := got[0]
	if c.Autonomy != "auto" || len(c.DependsOn) != 1 || c.DependsOn[0] != "database" || c.Body != "# X" {
		t.Errorf("cron parsed wrong: %+v", c)
	}
	if got[1].Autonomy != "approve" || got[1].Body != "# DB" {
		t.Errorf("database defaults wrong: %+v", got[1])
	}
}

func TestLoadHashIsFileBytes(t *testing.T) {
	dir := t.TempDir()
	content := "---\nautonomy: observe\n---\n# H\n"
	write(t, dir, "a.md", content)
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got[0].Hash != want {
		t.Errorf("hash = %s, want %s", got[0].Hash, want)
	}
}

func TestLoadRejectsUnknownFrontmatterKey(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\nautonomi: auto\n---\n# A")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "autonomi") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsBadAutonomy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\nautonomy: yolo\n---\n# A")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "yolo") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRejectsMissingDependency(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\ndepends_on: [nope]\n---\n# A")
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadExamples(t *testing.T) {
	got, err := Load("../../examples", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range got {
		names = append(names, i.Name)
	}
	if strings.Join(names, ",") != "app,cron,database" {
		t.Errorf("names = %v", names)
	}
}

// Ignored files are skipped by base name, in any case, before their name is
// checked: README.md is not an intent.
func TestLoadSkipsIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "# Project")
	write(t, dir, "Notes.md", "# Notes")
	write(t, dir, "app.md", "# App")
	got, err := Load(dir, "readme.md", "NOTES.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "app" {
		t.Errorf("intents = %+v, want only app", got)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "invalid intent name") {
		t.Errorf("without ignore: err = %v, want README.md and Notes.md rejected as intent names", err)
	} else if !strings.Contains(err.Error(), `pass it with --ignore`) {
		t.Errorf("err = %v, want the ignore hint", err)
	}
}

func TestLoadRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"App", "_apply", "-x", "web app", "a.b", "été"} {
		dir := t.TempDir()
		write(t, dir, name+".md", "# x")
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "invalid intent name") || !strings.Contains(err.Error(), name) {
			t.Errorf("%q: err = %v, want an invalid-name error naming it", name, err)
		}
	}
	dir := t.TempDir()
	for _, name := range []string{"web-app", "db_2", "0x"} {
		write(t, dir, name+".md", "# x")
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("valid names: %v", err)
	}
}

func TestLoadParsesAllowDestroy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\nautonomy: auto\nallow_destroy: true\n---\n# A")
	write(t, dir, "b.md", "---\nautonomy: auto\n---\n# B")
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].AllowDestroy || got[1].AllowDestroy {
		t.Errorf("AllowDestroy = %v, %v; want true for a, false (the default) for b", got[0].AllowDestroy, got[1].AllowDestroy)
	}
	write(t, dir, "c.md", "---\nallow_destroy: maybe\n---\n# C")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "c.md") {
		t.Errorf("allow_destroy: maybe: err = %v, want a frontmatter error", err)
	}
}
