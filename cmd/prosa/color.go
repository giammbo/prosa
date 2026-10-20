package main

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// ANSI codes of the CLI's own lines (the plan has its own, in engine).
const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiGreen = "\x1b[32m"
)

func paint(on bool, code, text string) string {
	if !on {
		return text
	}
	return code + text + ansiReset
}

// useColor reports whether the output to stdout gets ANSI colors: only on a
// terminal, never with --no-color nor with NO_COLOR set to a non-empty value
// (no-color.org).
func useColor(noColor bool, stdout io.Writer) bool {
	return !noColor && os.Getenv("NO_COLOR") == "" && isTerminal(stdout)
}

// isTerminal reports whether v, a reader or a writer, is a terminal; a
// variable for the tests.
var isTerminal = func(v any) bool {
	f, ok := v.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}
