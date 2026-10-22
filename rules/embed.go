package rules

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed aws
var builtinFS embed.FS

// Builtin returns the rules shipped in the binary, sorted by ID. Each file
// is named after its rule ID, sits in the directory of the ID's lower-case
// prefix, and cites at least one source.
func Builtin() ([]Rule, error) {
	var out []Rule
	err := fs.WalkDir(builtinFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return err
		}
		data, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		r, err := Parse("prosa", data)
		if err != nil {
			return fmt.Errorf("builtin %s: %w", p, err)
		}
		if want := r.ID + ".md"; path.Base(p) != want {
			return fmt.Errorf("builtin %s: file name must be %s", p, want)
		}
		if want := strings.ToLower(r.ID[:strings.Index(r.ID, "-")]); path.Dir(p) != want {
			return fmt.Errorf("builtin %s: %s belongs in %s/", p, r.ID, want)
		}
		if len(r.Sources) == 0 {
			return fmt.Errorf("builtin %s: no source", p)
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
