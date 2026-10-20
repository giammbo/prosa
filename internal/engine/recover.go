package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/state"
)

// Recovered is an intent whose lock entry Recover rebuilt: its number of
// resources and the serial of the state it was rebuilt from.
type Recovered struct {
	Intent    string
	Resources int
	Serial    int64
}

// aheadOfLock reports whether the state of an intent (st, with digest) was
// saved after the lock's reference to it: the digests differ and the
// state's serial is higher than the lock's (0 when li, the lock entry, is
// nil). It is what a stop between a state save and the lock save leaves.
func aheadOfLock(li *lock.Intent, st *state.IntentState, digest string) bool {
	want, serial := "", int64(0)
	if li != nil {
		want, serial = li.State.Digest, li.State.Serial
	}
	return digest != want && st.Serial > serial
}

// stateMismatch returns why the state of an intent does not match its lock
// entry li (nil when absent), "" when it does.
func stateMismatch(name string, li *lock.Intent, st *state.IntentState, digest, recoverCmd string) string {
	want, serial := "", int64(0)
	if li != nil {
		want, serial = li.State.Digest, li.State.Serial
	}
	switch {
	case digest == want:
		return ""
	case st.Serial > serial:
		return fmt.Sprintf(`state of intent %q is ahead of the lock (serial %d > %d): run "%s"`, name, st.Serial, serial, recoverCmd)
	}
	return fmt.Sprintf("state digest mismatch for intent %q", name)
}

// rebuildResources returns the lock resources of an intent rebuilt from its
// state st, sorted by key, given the lock's old ones. A record written before
// records kept their lock entry takes the id from its state, and chosen and
// depends_on from the old entry of its key. An old entry whose key the store
// does not have is kept without an id: the destroy half of a replace was
// saved, the create was not (Apply's forget keeps it the same way); were it
// a completed delete, the next Apply drops it, as the intent is planned
// again.
func rebuildResources(st *state.IntentState, old []lock.Resource) []lock.Resource {
	byKey := map[string]lock.Resource{}
	for _, r := range old {
		byKey[r.Key] = r
	}
	var out []lock.Resource
	for key, rs := range st.Resources {
		r := lock.Resource{Key: key, Provider: rs.Provider, Type: rs.Type, ID: rs.ID, Chosen: rs.Chosen, DependsOn: rs.DependsOn}
		if rs.ID == "" && rs.Chosen == nil && rs.DependsOn == nil { // an old record
			r.ID = idOf(rs.State)
			if o, ok := byKey[key]; ok {
				r.Chosen, r.DependsOn = o.Chosen, o.DependsOn
			}
		}
		out = append(out, r)
	}
	for key, o := range byKey {
		if st.Resources[key] == nil {
			o.ID = ""
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b lock.Resource) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Recover rebuilds, from the state store, the lock entry of every intent
// whose state is ahead of the lock (ADR-024; see aheadOfLock), as Plan
// checks them, also when the lock is missing or has no intents (a deleted
// lock, or the lock of another folder's environment of the same name). Each rebuilt entry has the
// resources of the store with their provider, type, id, chosen and
// depends_on (see rebuildResources), the store's serial and digest as its
// state, and an empty intent_hash, so that the intent is planned again (or
// resumed from the Pending desired state the store keeps for it); its
// recipe, depends_on, checks and reconciled_at are kept. Recover holds the
// project lock and the lock of every intent, like Apply, and writes the lock
// only when it rebuilt something; it returns the rebuilt intents, sorted.
func (e *Engine) Recover() ([]Recovered, error) {
	unlockApply, err := e.State.Lock(applyLockName)
	if err != nil {
		return nil, kindErr(KindLock, "another apply is running", fmt.Errorf("another apply is running: %w", err))
	}
	defer unlockApply()
	intents, err := intent.Load(e.Dir, e.Ignore...)
	if err != nil {
		return nil, err
	}
	lk, err := lock.Load(e.LockPath)
	if err != nil {
		return nil, err
	}
	if lk.Intents == nil {
		lk.Intents = map[string]*lock.Intent{}
	}
	set := map[string]bool{}
	for _, it := range intents {
		set[it.Name] = true
	}
	for name := range lk.Intents {
		set[name] = true
	}
	names := slices.Sorted(maps.Keys(set))
	unlock, err := e.lockIntents(names)
	if err != nil {
		return nil, err
	}
	defer unlock()

	var out []Recovered
	for _, name := range names {
		st, digest, err := e.State.Load(name)
		if err != nil {
			return nil, err
		}
		li := lk.Intents[name]
		if !aheadOfLock(li, st, digest) {
			continue
		}
		if li == nil {
			li = &lock.Intent{}
			lk.Intents[name] = li
		}
		li.IntentHash = ""
		li.State = lock.StateRef{Serial: st.Serial, Digest: digest}
		li.Resources = rebuildResources(st, li.Resources)
		out = append(out, Recovered{Intent: name, Resources: len(li.Resources), Serial: st.Serial})
	}
	if len(out) == 0 {
		return nil, nil
	}
	if _, err := lock.Save(e.LockPath, lk); err != nil {
		return nil, kindErr(KindStateStore, "lock write failed", fmt.Errorf("save lock: %w", err))
	}
	return out, nil
}
