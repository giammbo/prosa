package engine

import "strings"

// ANSI codes of the plan output.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// style paints text with ANSI codes when on, and leaves it as is otherwise.
type style struct{ on bool }

func (s style) paint(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return code + text + ansiReset
}

// symbol colors an action symbol: + green, ~ yellow, - red; in -/+ the -
// red and the + green.
func (s style) symbol(sym string) string {
	var b strings.Builder
	for _, r := range sym {
		switch r {
		case '+':
			b.WriteString(s.paint(ansiGreen, "+"))
		case '-':
			b.WriteString(s.paint(ansiRed, "-"))
		case '~':
			b.WriteString(s.paint(ansiYellow, "~"))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
