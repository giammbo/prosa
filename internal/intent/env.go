package intent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Env is an environment file (ADR-029): prose that wins over what the
// intents of the project say, passed with --env. The name of the file,
// without ".md", is the name of the environment.
type Env struct {
	Name string // file name without ".md"
	Path string // absolute
	Body string
	Hash string // "sha256:" + hex(sha256(file bytes)), as Intent.Hash
}

// LoadEnv reads the environment file at path. Its name follows the rule of
// the intents' names; unlike an intent it has no frontmatter.
func LoadEnv(path string) (Env, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Env{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Env{}, err
	}
	name, ok := strings.CutSuffix(filepath.Base(abs), ".md")
	if !ok || !validName.MatchString(name) {
		return Env{}, fmt.Errorf("%s: invalid environment name %q: the file must be <name>.md with a name matching %s", abs, strings.TrimSuffix(filepath.Base(abs), ".md"), validName)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		return Env{}, fmt.Errorf("%s: environment files have no frontmatter", abs)
	}
	sum := sha256.Sum256(data)
	return Env{Name: name, Path: abs, Body: strings.TrimSpace(text), Hash: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// sections splits the body of the environment: the lines outside any
// "## <intent>" section (the "#" title excluded), and the text of each
// section, all trimmed. A heading names its intent case-insensitively: the
// sections are keyed by the lower-case heading.
func (e Env) sections() (string, map[string]string) {
	var common []string
	secs := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(e.Body, "\n") {
		if name, ok := strings.CutPrefix(line, "## "); ok {
			cur = strings.ToLower(strings.TrimSpace(name))
			continue
		}
		if strings.HasPrefix(line, "# ") {
			continue
		}
		if cur == "" {
			common = append(common, line)
		} else {
			secs[cur] = append(secs[cur], line)
		}
	}
	out := make(map[string]string, len(secs))
	for k, v := range secs {
		out[k] = strings.TrimSpace(strings.Join(v, "\n"))
	}
	return strings.TrimSpace(strings.Join(common, "\n")), out
}

// UnknownSections returns the "## X" headings (as written, trimmed) that name
// none of the intents names (case-insensitively): their lines reach no
// intent. A heading repeated is listed once.
func (e Env) UnknownSections(names []string) []string {
	known := map[string]bool{}
	for _, n := range names {
		known[strings.ToLower(n)] = true
	}
	var out []string
	for _, line := range strings.Split(e.Body, "\n") {
		name, ok := strings.CutPrefix(line, "## ")
		name = strings.TrimSpace(name)
		if !ok || known[strings.ToLower(name)] || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Common is the part of the environment that every intent sees: the lines
// outside any "## <intent>" section, without the "#" title.
func (e Env) Common() string {
	c, _ := e.sections()
	return c
}

// Section is the text under "## <intent>" (the heading in any case), ""
// when the environment has none for that intent.
func (e Env) Section(intent string) string {
	_, s := e.sections()
	return s[strings.ToLower(intent)]
}

// EnvFor is the text of the environment that concerns one intent: the common
// lines, then its own section under its heading. It is what the planner is
// shown and what the state keeps as the previous environment.
func EnvFor(e Env, intent string) string {
	c, secs := e.sections()
	if s := secs[strings.ToLower(intent)]; s != "" {
		if c != "" {
			c += "\n\n"
		}
		c += "## " + intent + "\n" + s
	}
	return c
}
