package provider

import (
	"testing"

	"github.com/giammbo/prosa/internal/tfplugin5"
)

func TestDiagsErrOnlyErrors(t *testing.T) {
	warn := &tfplugin5.Diagnostic{Severity: tfplugin5.Diagnostic_WARNING, Summary: "careful", Detail: "w"}
	bad := &tfplugin5.Diagnostic{Severity: tfplugin5.Diagnostic_ERROR, Summary: "bad", Detail: "x"}

	err := diagsErr("plan", []*tfplugin5.Diagnostic{warn, bad})
	if err == nil || err.Error() != "plan: bad: x" {
		t.Fatalf("got %v, want %q", err, "plan: bad: x")
	}
	if err := diagsErr("plan", []*tfplugin5.Diagnostic{warn}); err != nil {
		t.Fatalf("warnings only: got %v, want nil", err)
	}
	if err := diagsErr("plan", nil); err != nil {
		t.Fatalf("no diagnostics: got %v, want nil", err)
	}

	two := diagsErr("apply", []*tfplugin5.Diagnostic{bad, {Severity: tfplugin5.Diagnostic_ERROR, Summary: "worse", Detail: "y"}})
	if two == nil || two.Error() != "apply: bad: x\napply: worse: y" {
		t.Fatalf("two errors: got %q", two)
	}
}
