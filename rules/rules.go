// Package rules holds the best-practice rules: prose with a few front matter
// keys, shipped in the binary or written by the project (ADR-034).
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Source is the reference a rule rests on.
type Source struct {
	Label string `yaml:"label"`
	URL   string `yaml:"url"`
}

// Rule is one best-practice rule.
type Rule struct {
	ID        string
	Title     string
	AppliesTo []string // resource types the rule is about
	Sources   []Source
	Body      string // prose, trimmed
	Origin    string // "prosa" for built-ins, else the path relative to the project root
}

type frontmatter struct {
	ID        string   `yaml:"id"`
	Title     string   `yaml:"title"`
	AppliesTo []string `yaml:"applies_to"`
	Source    []Source `yaml:"source"`
}

// validID is the form of a rule ID: <PREFIX>-<AREA>-<NNN>, upper case.
const idPattern = `[A-Z][A-Z0-9]*-[A-Z][A-Z0-9]*-[0-9]{3}`

var validID = regexp.MustCompile(`^` + idPattern + `$`)

// Parse reads one rule: front matter between "---" lines, then the prose.
// origin names the file in errors and becomes Rule.Origin.
func Parse(origin string, data []byte) (Rule, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Rule{}, fmt.Errorf("%s: missing front matter", origin)
	}
	var fm, body string
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		fm, body = rest[:i], rest[i+len("\n---\n"):]
	} else if strings.HasSuffix(rest, "\n---") {
		fm = strings.TrimSuffix(rest, "\n---")
	} else {
		return Rule{}, fmt.Errorf("%s: unterminated front matter", origin)
	}
	var f frontmatter
	dec := yaml.NewDecoder(bytes.NewReader([]byte(fm)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return Rule{}, fmt.Errorf("%s: front matter: %w", origin, err)
	}
	if !validID.MatchString(f.ID) {
		return Rule{}, fmt.Errorf("%s: invalid id %q: must match %s", origin, f.ID, validID)
	}
	if f.Title == "" {
		return Rule{}, fmt.Errorf("%s: missing title", origin)
	}
	if len(f.AppliesTo) == 0 {
		return Rule{}, fmt.Errorf("%s: applies_to is empty", origin)
	}
	r := Rule{ID: f.ID, Title: f.Title, AppliesTo: f.AppliesTo, Sources: f.Source, Body: strings.TrimSpace(body), Origin: origin}
	if r.Body == "" {
		return Rule{}, fmt.Errorf("%s: empty body", origin)
	}
	return r, nil
}

// Hash is "sha256:" + hex of the JSON of the rules' id, applies_to and body,
// sorted by ID: what changes the plan, not titles or sources.
func Hash(rs []Rule) string {
	type entry struct {
		ID        string   `json:"id"`
		AppliesTo []string `json:"applies_to"`
		Body      string   `json:"body"`
	}
	es := make([]entry, 0, len(rs))
	for _, r := range rs {
		es = append(es, entry{r.ID, r.AppliesTo, r.Body})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
	b, _ := json.Marshal(es)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Exception is a paragraph of an intent that names a rule: the user's
// reason for departing from it.
type Exception struct {
	ID, Text string
}

var idToken = regexp.MustCompile(`\b` + idPattern + `\b`)

// Exceptions finds the paragraphs (separated by blank lines) of text that
// name a rule ID. A known ID gives one Exception per paragraph; an ID token
// not in known goes to unknown, once.
func Exceptions(text string, known []string) (ex []Exception, unknown []string) {
	isKnown := map[string]bool{}
	for _, id := range known {
		isKnown[id] = true
	}
	seen := map[string]bool{}
	for _, p := range regexp.MustCompile(`\n\s*\n`).Split(strings.ReplaceAll(text, "\r\n", "\n"), -1) {
		p = strings.TrimSpace(p)
		inPara := map[string]bool{}
		for _, id := range idToken.FindAllString(p, -1) {
			switch {
			case inPara[id]:
			case isKnown[id]:
				inPara[id] = true
				ex = append(ex, Exception{ID: id, Text: p})
			case !seen[id]:
				seen[id] = true
				unknown = append(unknown, id)
			}
		}
	}
	return ex, unknown
}
