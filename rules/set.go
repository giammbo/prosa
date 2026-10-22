package rules

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const disabledFile = "rules/disabled.txt"

// LoadProject reads the project's own rules, every rules/**/*.md under root
// except README.md, and the IDs listed in rules/disabled.txt (one per line,
// "#" starts a comment). A project without rules/ is not an error.
func LoadProject(root string) (rs []Rule, disabled []string, err error) {
	dir := filepath.Join(root, "rules")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil, nil
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".md" || strings.EqualFold(d.Name(), "README.md") {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		r, err := Parse(filepath.ToSlash(rel), data)
		if err != nil {
			return err
		}
		rs = append(rs, r)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	data, err := os.ReadFile(filepath.Join(dir, "disabled.txt"))
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			disabled = append(disabled, strings.ToUpper(line))
		}
	}
	return rs, disabled, nil
}

// DisabledRule is a rule switched off, and where from.
type DisabledRule struct {
	ID string
	By string // "rules/disabled.txt" or "--disable-rule"
}

// Set is the rules in force for one run.
type Set struct {
	Active   []Rule // sorted by ID
	Disabled []DisabledRule
	Warnings []string
}

// NewSet merges the built-in and project rules and applies the disabled
// IDs. A duplicate ID is an error; a disabled ID that matches no rule is a
// warning. IDs from the flag are upper-cased.
func NewSet(builtin, project []Rule, fromFile, disabledFlag []string) (Set, error) {
	var s Set
	all := map[string]Rule{}
	for _, r := range slices.Concat(builtin, project) {
		if prev, dup := all[r.ID]; dup {
			return Set{}, fmt.Errorf("%s: rule %s already defined by %s", r.Origin, r.ID, prev.Origin)
		}
		all[r.ID] = r
	}
	off := map[string]bool{}
	disable := func(ids []string, by string) {
		for _, id := range ids {
			id = strings.ToUpper(id)
			if _, ok := all[id]; !ok {
				s.Warnings = append(s.Warnings, fmt.Sprintf("unknown rule %s in %s", id, by))
			} else if !off[id] {
				off[id] = true
				s.Disabled = append(s.Disabled, DisabledRule{ID: id, By: by})
			}
		}
	}
	disable(fromFile, disabledFile)
	disable(disabledFlag, "--disable-rule")
	for id, r := range all {
		if !off[id] {
			s.Active = append(s.Active, r)
		}
	}
	sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].ID < s.Active[j].ID })
	return s, nil
}

// For returns the active rules whose AppliesTo meets types, sorted by ID.
func (s Set) For(types []string) []Rule {
	var out []Rule
	for _, r := range s.Active {
		if slices.ContainsFunc(r.AppliesTo, func(t string) bool { return slices.Contains(types, t) }) {
			out = append(out, r)
		}
	}
	return out
}

// IDs returns the IDs of the active rules.
func (s Set) IDs() []string {
	out := make([]string, 0, len(s.Active))
	for _, r := range s.Active {
		out = append(out, r.ID)
	}
	return out
}
