package engine

import (
	"fmt"
	"sort"
	"strings"
)

// TopoSort orders the nodes of deps (node -> nodes it depends on) so that
// dependencies come first. Among ready nodes the order is lexicographic.
// Nodes that only appear as dependencies are included. A cycle is an error
// listing the nodes of the cycle.
func TopoSort(deps map[string][]string) ([]string, error) {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for n, ds := range deps {
		if _, ok := indeg[n]; !ok {
			indeg[n] = 0
		}
		uniq := map[string]bool{}
		for _, d := range ds {
			if uniq[d] {
				continue
			}
			uniq[d] = true
			if _, ok := indeg[d]; !ok {
				indeg[d] = 0
			}
			indeg[n]++
			dependents[d] = append(dependents[d], n)
		}
	}
	var ready []string
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	var order []string
	for len(ready) > 0 {
		sort.Strings(ready)
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		for _, m := range dependents[n] {
			indeg[m]--
			if indeg[m] == 0 {
				ready = append(ready, m)
			}
		}
	}
	if len(order) == len(indeg) {
		return order, nil
	}
	return nil, &CycleError{Nodes: findCycle(deps, indeg)}
}

// CycleError is a dependency cycle; Nodes lists it with the first node
// repeated at the end.
type CycleError struct{ Nodes []string }

func (e *CycleError) Error() string {
	return fmt.Sprintf("dependency cycle: %s", strings.Join(e.Nodes, " -> "))
}

// findCycle returns one cycle (first node repeated at the end) among the
// nodes still blocked after Kahn's algorithm, deterministically.
func findCycle(deps map[string][]string, indeg map[string]int) []string {
	var blocked []string
	for n, d := range indeg {
		if d > 0 {
			blocked = append(blocked, n)
		}
	}
	sort.Strings(blocked)
	isBlocked := map[string]bool{}
	for _, n := range blocked {
		isBlocked[n] = true
	}
	state := map[string]int{} // 1 = on stack, 2 = done
	var stack []string
	var found []string
	var visit func(n string) bool
	visit = func(n string) bool {
		state[n] = 1
		stack = append(stack, n)
		next := append([]string(nil), deps[n]...)
		sort.Strings(next)
		for _, d := range next {
			if !isBlocked[d] {
				continue
			}
			if state[d] == 1 {
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				found = append(append([]string(nil), stack[i:]...), d)
				return true
			}
			if state[d] == 0 && visit(d) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = 2
		return false
	}
	for _, n := range blocked {
		if state[n] == 0 && visit(n) {
			return found
		}
	}
	return blocked
}
