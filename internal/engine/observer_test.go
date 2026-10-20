package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/giammbo/prosa/internal/provider"
)

func TestClassifyIsValueFree(t *testing.T) {
	const secret = "SECRET-NAME"
	d1 := &provider.DiagError{Op: "apply", Sum: "exists " + secret, Detail: secret, Attr: "bucket"}
	d2 := &provider.DiagError{Op: "apply", Sum: secret, Detail: secret, Attr: "tags[\"" + secret + "\"]"}
	d3 := &provider.DiagError{Op: "apply", Sum: secret, Attr: "environment.variables[\"" + secret + "\"].x"}
	d4 := &provider.DiagError{Op: "apply", Sum: secret, Attr: "rule[0].tags[\"team\"]"}
	d5 := &provider.DiagError{Op: "apply", Sum: secret, Attr: "tags[\"other\"]"}
	ve := &ValidationError{Problems: []ValidationProblem{{Intent: "a", Key: "k", Message: "bad " + secret}}}
	cases := []struct {
		name string
		err  error
		want ErrorInfo
	}{
		// Attribute paths stop at the first map key, which can be a value.
		{"provider", errors.Join(fmt.Errorf("app/b: %w", d1), d2, d3, d4, d5),
			ErrorInfo{Kind: KindProvider, Message: "apply: provider error", Attributes: []string{"bucket", "tags[*]", "environment.variables[*]", "rule[0].tags[*]"}, Count: 5}},
		{"validation", fmt.Errorf("planner: %w", ve), ErrorInfo{Kind: KindValidation, Message: "1 problem(s)", Count: 1}},
		{"canceled", fmt.Errorf("x: %w", context.Canceled), ErrorInfo{Kind: KindInterrupted, Message: "interrupted"}},
		{"not approved", ErrNotApproved, ErrorInfo{Kind: KindApproval, Message: "plan not approved"}},
		{"reference", refErr("app/b", errors.New("template "+secret)), ErrorInfo{Kind: KindInternal, Message: "app/b: reference error"}},
		{"planner", &KindError{Kind: KindPlanner, Msg: "no valid desired state after 3 turns: 2 problem(s)", Err: errors.New(secret)},
			ErrorInfo{Kind: KindPlanner, Message: "no valid desired state after 3 turns: 2 problem(s)"}},
		{"unclassified", errors.New("boom " + secret), ErrorInfo{Kind: KindInternal, Message: "error (see command output)"}},
	}
	for _, tc := range cases {
		got := Classify(tc.err)
		if !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("%s: Classify = %+v, want %+v", tc.name, *got, tc.want)
		}
		if strings.Contains(fmt.Sprint(*got), secret) {
			t.Errorf("%s: classification holds the secret: %+v", tc.name, *got)
		}
	}
	if Classify(nil) != nil {
		t.Error("nil error must classify to nil")
	}
}
