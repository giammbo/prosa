package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var errBoom = errors.New("boom")

func TestTopoSortDeterministic(t *testing.T) {
	got, err := TopoSort(map[string][]string{"c": {"a"}, "b": {"a"}, "a": nil})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %v", got)
	}
}

func TestTopoSortImplicitNode(t *testing.T) {
	got, err := TopoSort(map[string][]string{"b": {"a"}})
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v err=%v", got, err)
	}
}

func TestTopoSortCycle(t *testing.T) {
	_, err := TopoSort(map[string][]string{"a": {"b"}, "b": {"a"}, "z": nil})
	if err == nil || !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "z") {
		t.Fatalf("z is not in the cycle: %v", err)
	}
}
