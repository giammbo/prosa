package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/giammbo/prosa/internal/project"
)

// initCommand runs prosa init: it makes the directory given, the current
// one by default, a project (project.Init) and says what it did.
func initCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		usage(stderr)
		return 2
	}
	root, err := filepath.Abs(fs.Arg(0)) // "" is the current directory
	if err == nil {
		if _, oerr := project.Open(root); errors.Is(oerr, project.ErrLegacyConfig) {
			fmt.Fprintf(stderr, "prosa: %v\n", oerr)
			return 2
		}
		var done []string
		done, err = project.Init(root)
		for _, line := range done {
			fmt.Fprintln(stdout, line)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return 1
	}
	return 0
}
