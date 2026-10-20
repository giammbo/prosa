package provider

import (
	"errors"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

// Diagnostic is a provider diagnostic. Severity is "error" or "warning";
// Attribute is the attribute path it refers to (as name, tags["team"],
// setting[0].value), empty when it refers to the whole resource.
type Diagnostic struct {
	Severity                   string
	Summary, Detail, Attribute string
}

// DiagError is one error diagnostic of operation Op. Its text is
// "<op>: <summary>[: <detail>]"; Attr is the attribute path it refers to (empty for the whole
// resource). Detail and Sum may quote values: the event log keeps neither.
type DiagError struct{ Op, Sum, Detail, Attr string }

func (e *DiagError) Error() string {
	msg := e.Op + ": " + e.Sum
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Summary returns "<op>: <summary>".
func (e *DiagError) Summary() string { return e.Op + ": " + e.Sum }

// diagsErr returns an error for the error-severity diagnostics of operation
// op, formatted as "<op>: <summary>: <detail>" and joined with errors.Join.
// Warnings are ignored; it returns nil when there are no errors. It is the
// only way provider error diagnostics become Go errors.
func diagsErr(op string, ds []*tfplugin5.Diagnostic) error {
	var errs []error
	for _, d := range ds {
		if d.GetSeverity() != tfplugin5.Diagnostic_ERROR {
			continue
		}
		errs = append(errs, &DiagError{Op: op, Sum: d.GetSummary(), Detail: d.GetDetail(), Attr: pathString(d.GetAttribute())})
	}
	return errors.Join(errs...)
}

// diagsWarnings returns the warning-severity diagnostics, nil when there are
// none.
func diagsWarnings(ds []*tfplugin5.Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range ds {
		if d.GetSeverity() != tfplugin5.Diagnostic_WARNING {
			continue
		}
		out = append(out, Diagnostic{
			Severity:  "warning",
			Summary:   d.GetSummary(),
			Detail:    d.GetDetail(),
			Attribute: pathString(d.GetAttribute()),
		})
	}
	return out
}
