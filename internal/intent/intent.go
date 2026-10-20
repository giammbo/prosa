// Package intent loads the prose intents (Markdown files with optional YAML frontmatter).
package intent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Intent is one prose intent file.
type Intent struct {
	Name      string // file name without ".md"
	Path      string
	Hash      string // "sha256:" + hex(sha256(file bytes))
	Autonomy  string // observe|advise|approve|auto
	DependsOn []string
	// AllowDestroy lets deletes and replaces of the intent's resources go
	// without approval when its autonomy is auto (ADR-022).
	AllowDestroy bool
	Body         string // text without frontmatter
}

type frontmatter struct {
	Autonomy     string   `yaml:"autonomy"`
	DependsOn    []string `yaml:"depends_on"`
	AllowDestroy bool     `yaml:"allow_destroy"`
}

// validName is the form of an intent name: lower case, so that names never
// collide on a case-insensitive file system, and never starting with "_" or
// "-", which the engine keeps for its own files.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var validAutonomy = map[string]bool{"observe": true, "advise": true, "approve": true, "auto": true}

// Load reads every *.md at the first level of dir, sorted by Name, except
// the files named in ignore (base names, compared without regard to case:
// a project's README.md is not an intent, ADR-027).
func Load(dir string, ignore ...string) ([]Intent, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	var out []Intent
	for _, p := range paths {
		if slices.ContainsFunc(ignore, func(name string) bool { return strings.EqualFold(name, filepath.Base(p)) }) {
			continue
		}
		it, err := loadOne(p)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	names := map[string]bool{}
	for _, it := range out {
		names[it.Name] = true
	}
	for _, it := range out {
		for _, d := range it.DependsOn {
			if !names[d] {
				return nil, fmt.Errorf("%s: depends_on %q: no such intent", it.Path, d)
			}
		}
	}
	return out, nil
}

func loadOne(path string) (Intent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Intent{}, err
	}
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	if !validName.MatchString(name) {
		return Intent{}, fmt.Errorf("%s: invalid intent name %q: must match %s (not an intent? pass it with --ignore)", path, name, validName)
	}
	sum := sha256.Sum256(data)
	it := Intent{
		Name:     name,
		Path:     path,
		Hash:     "sha256:" + hex.EncodeToString(sum[:]),
		Autonomy: "approve",
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	body := text
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		var fm, after string
		if i := strings.Index(rest, "\n---\n"); i >= 0 {
			fm, after = rest[:i], rest[i+len("\n---\n"):]
		} else if strings.HasSuffix(rest, "\n---") {
			fm, after = strings.TrimSuffix(rest, "\n---"), ""
		} else {
			return Intent{}, fmt.Errorf("%s: unterminated frontmatter", path)
		}
		var f frontmatter
		dec := yaml.NewDecoder(bytes.NewReader([]byte(fm)))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
			return Intent{}, fmt.Errorf("%s: frontmatter: %w", path, err)
		}
		if f.Autonomy != "" {
			if !validAutonomy[f.Autonomy] {
				return Intent{}, fmt.Errorf("%s: invalid autonomy %q (want observe|advise|approve|auto)", path, f.Autonomy)
			}
			it.Autonomy = f.Autonomy
		}
		it.DependsOn, it.AllowDestroy = f.DependsOn, f.AllowDestroy
		body = after
	}
	it.Body = strings.TrimSpace(body)
	return it, nil
}
