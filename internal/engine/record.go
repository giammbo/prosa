package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/state"
)

// resourceState is the state-store record of c's resource with state v,
// with the chosen, depends_on and id of its lock entry (ADR-024).
func (a *applier) resourceState(c *Change, v cty.Value, private []byte, identity cty.Value) (*state.ResourceState, error) {
	rsch := a.p.schema.Resources[c.Type]
	js, err := ctyjson.Marshal(v, rsch.Block.ImpliedType())
	if err != nil {
		return nil, fmt.Errorf("%s/%s: new state: %w", c.Intent, c.Key, err)
	}
	rs := &state.ResourceState{Provider: c.provider, Type: c.Type, SchemaVersion: rsch.Version, State: js, Private: private,
		Chosen: c.config, DependsOn: c.dependsOn, ID: idOf(js)}
	if is := a.p.schema.Identities[c.Type]; is != nil && identity != cty.NilVal && !identity.IsNull() {
		if rs.Identity, err = ctyjson.Marshal(identity, is.Type); err != nil {
			return nil, fmt.Errorf("%s/%s: new identity: %w", c.Intent, c.Key, err)
		}
		rs.IdentityVersion = is.Version
	}
	return rs, nil
}

// save records the state of c's resource in the store and in the lock.
func (a *applier) save(c *Change, v cty.Value, private []byte, identity cty.Value) error {
	rs, err := a.resourceState(c, v, private, identity)
	if err != nil {
		return err
	}
	a.p.states[c.Intent].Resources[c.Key] = rs
	li := a.lockIntent(c.Intent)
	setLockResource(li, lock.Resource{Key: c.Key, Provider: c.provider, Type: c.Type, ID: rs.ID, Chosen: rs.Chosen, DependsOn: rs.DependsOn})
	return a.commit(c.Intent)
}

// catchUp sets, in the intent's state, the refreshed state of its no-op and
// skipped resources when it differs from the stored one (ADR-018). It reports
// whether the state changed; the caller saves it.
func (a *applier) catchUp(name string) (bool, error) {
	st := a.p.states[name]
	dirty := false
	for i := range a.p.Changes {
		c := &a.p.Changes[i]
		if c.Intent != name || c.prior.IsNull() || !(c.Action == NoOp || a.skipped[name+"/"+c.Key]) {
			continue
		}
		v, _, err := stripWriteOnly(a.p.schema.Resources[c.Type].Block, c.prior)
		if err != nil {
			return false, fmt.Errorf("%s/%s: %w", name, c.Key, err)
		}
		rs, err := a.resourceState(c, v, c.priorPrivate, c.priorIdentity)
		if err != nil {
			return false, err
		}
		if sameResourceState(st.Resources[c.Key], rs) {
			continue
		}
		st.Resources[c.Key] = rs
		dirty = true
	}
	return dirty, nil
}

// sameResourceState reports whether two records are equal (nil and empty
// byte fields are the same).
func sameResourceState(a, b *state.ResourceState) bool {
	return a != nil && b != nil && a.Provider == b.Provider && a.Type == b.Type &&
		a.SchemaVersion == b.SchemaVersion && bytes.Equal(a.State, b.State) &&
		bytes.Equal(a.Private, b.Private) && bytes.Equal(a.Identity, b.Identity) &&
		a.IdentityVersion == b.IdentityVersion && sameLockEntry(a, b.Chosen, b.DependsOn, b.ID)
}

// sameLockEntry reports whether rs holds the lock entry chosen, dependsOn,
// id; chosen is compared as JSON, where numbers of any Go type are digits.
func sameLockEntry(rs *state.ResourceState, chosen map[string]any, dependsOn []string, id string) bool {
	if rs.ID != id || !slices.Equal(rs.DependsOn, dependsOn) {
		return false
	}
	a, errA := json.Marshal(rs.Chosen)
	b, errB := json.Marshal(chosen)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// forget drops c's resource from the store. A deleted resource also leaves
// the lock; a replaced one keeps its entry, without an id, so that a failed
// create is planned again.
func (a *applier) forget(c *Change) error {
	a.effects++
	delete(a.p.states[c.Intent].Resources, c.Key)
	li := a.lockIntent(c.Intent)
	if c.Action == Delete {
		li.Resources = slices.DeleteFunc(li.Resources, func(r lock.Resource) bool { return r.Key == c.Key })
	} else {
		setLockResource(li, lock.Resource{Key: c.Key, Provider: c.provider, Type: c.Type, Chosen: c.config, DependsOn: c.dependsOn})
	}
	return a.commit(c.Intent)
}

// commit saves the state of an intent and the lock with its new reference.
// The state of an intent of Dir that changed since the lock (or is new)
// also keeps the desired state being applied, as Pending (ADR-024).
func (a *applier) commit(name string) error {
	a.executed[name] = true
	st := a.p.states[name]
	if a.stale[name] {
		if err := a.setPending(name, st); err != nil {
			return err
		}
	}
	digest, err := a.e.State.Save(name, st)
	if err != nil {
		return fmt.Errorf("save state of intent %q: %w", name, err)
	}
	a.p.Written = true
	li := a.lockIntent(name)
	li.State = lock.StateRef{Serial: st.Serial, Digest: digest}
	if a.changed[name] {
		li.IntentHash = ""
	}
	return a.saveLock()
}

// setPending sets in st, the state of intent name of Dir, the desired state
// Apply executes for it, with the hash of its text.
func (a *applier) setPending(name string, st *state.IntentState) error {
	i := slices.IndexFunc(a.p.intents, func(it intent.Intent) bool { return it.Name == name })
	if i < 0 {
		return nil
	}
	js, err := json.Marshal(Desired{Resources: a.p.desired[name], Checks: a.p.checks[name], Patch: a.p.patches[name]})
	if err != nil {
		return fmt.Errorf("intent %q: pending desired state: %w", name, err)
	}
	st.Pending = &state.Pending{IntentHash: a.p.intentHash(a.p.intents[i]), Desired: js}
	return nil
}

// finish completes an intent whose changes all succeeded: a removed intent
// leaves the lock and the store; any other gets its new hash, its desired
// resources and its checks in the lock, and in its state its text (Body, for the planner),
// no Pending and, when something was applied, the catch-up of its unchanged resources;
// the state is saved only if that changed it; with a reviewer, its rules_hash
// is the one the plan settled (settleRules), without one a rewritten intent
// loses it. An intent whose changes were
// all skipped and whose hash is unchanged writes nothing but a new
// rules_hash, and a held one nothing: what of it was applied is already in
// the lock and the state.
func (a *applier) finish(name string) error {
	if a.held[name] {
		return nil
	}
	if slices.Contains(a.p.removed, name) {
		delete(a.p.lock.Intents, name)
		if err := a.saveLock(); err != nil {
			return err
		}
		if err := a.e.State.Remove(name); err != nil {
			return err
		}
		if a.p.env == nil {
			return a.e.removeBaseText(name)
		}
		return nil
	}
	if !a.executed[name] && !a.stale[name] {
		return a.saveRulesHash(name)
	}
	i := slices.IndexFunc(a.p.intents, func(it intent.Intent) bool { return it.Name == name })
	it := a.p.intents[i]
	st := a.p.states[name]
	envBody := a.p.envBody(it.Name)
	dirty := st.Body != it.Body || st.EnvBody != envBody || st.Pending != nil
	st.Body, st.EnvBody, st.Pending = it.Body, envBody, nil
	if a.executed[name] {
		caught, err := a.catchUp(name)
		if err != nil {
			return err
		}
		dirty = dirty || caught
	}
	var resources []lock.Resource
	for _, r := range a.p.desired[name] {
		lr := lock.Resource{Key: r.Key, Provider: a.p.providerName, Type: r.Type, Chosen: r.Config, DependsOn: r.DependsOn}
		if rs := st.Resources[r.Key]; rs != nil {
			lr.ID = idOf(rs.State)
			// The record keeps the lock entry written with it (ADR-024).
			if !sameLockEntry(rs, lr.Chosen, lr.DependsOn, lr.ID) {
				rs.Chosen, rs.DependsOn, rs.ID = lr.Chosen, lr.DependsOn, lr.ID
				dirty = true
			}
		}
		resources = append(resources, lr)
	}
	if dirty {
		digest, err := a.e.State.Save(name, st)
		if err != nil {
			return fmt.Errorf("save state of intent %q: %w", name, err)
		}
		a.lockIntent(name).State = lock.StateRef{Serial: st.Serial, Digest: digest}
	}
	li := a.lockIntent(name)
	li.IntentHash, li.DependsOn, li.Resources = a.p.intentHash(it), it.DependsOn, resources
	li.Checks = a.p.checks[name]
	li.BaseHash, li.Patch = a.p.baseHashes[name], a.p.patches[name] // none without a base
	switch {
	case a.p.reviewer != nil:
		li.RulesHash = a.p.rulesHashes[name] // none when its state was not reviewed
	case a.stale[name]:
		li.RulesHash = "" // rewritten without a review: reviewed next time
	}
	li.ReconciledAt = a.now().UTC().Truncate(time.Second)
	if err := a.saveLock(); err != nil {
		return err
	}
	if a.p.env == nil {
		// The root's lock entry is the base: keep the text it was planned from.
		return a.e.writeBaseText(name, it.Body)
	}
	return nil
}

// saveRulesHash writes the rules_hash of name, an intent reused as it is,
// when it is its only change to the lock (rulesStale); nothing otherwise.
func (a *applier) saveRulesHash(name string) error {
	if !a.p.rulesStale(name) {
		return nil
	}
	a.lockIntent(name).RulesHash = a.p.rulesHashes[name]
	return a.saveLock()
}

func (a *applier) now() time.Time {
	if a.e.Now != nil {
		return a.e.Now()
	}
	return time.Now()
}

// lockIntent returns the lock entry of an intent, creating it (with an empty
// intent_hash) for a new one.
func (a *applier) lockIntent(name string) *lock.Intent {
	li := a.p.lock.Intents[name]
	if li == nil {
		li = &lock.Intent{}
		a.p.lock.Intents[name] = li
	}
	return li
}

// saveLock pins the provider version used and writes the lock if it changed.
func (a *applier) saveLock() error {
	if a.p.providerVersion != "" {
		a.p.lock.ProviderVersions[a.p.providerName] = a.p.providerVersion
	}
	changed, err := lock.Save(a.e.LockPath, a.p.lock)
	if err != nil {
		return fmt.Errorf("save lock: %w", err)
	}
	a.p.Written = a.p.Written || changed
	return nil
}

// saveBase writes, in the base lock, the bases this plan planned for the
// intents the root workspace has not applied (baseWrite), provided the file
// did not change since the plan, and keeps the text each was planned from as
// its base text; the other entries of the base lock are left as they are on
// disk. The provider version used is pinned when the base lock has none.
func (a *applier) saveBase() error {
	p := a.p
	if len(p.baseWrite) == 0 {
		return nil
	}
	data, err := os.ReadFile(a.e.BaseLockPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(data, p.baseBytes) {
		return kindErr(KindLock, "base lock changed since plan", errors.New("base lock changed since plan; run plan again"))
	}
	disk, _, err := lock.Read(a.e.BaseLockPath)
	if err != nil {
		return err
	}
	for name := range p.baseWrite {
		disk.Intents[name] = p.base.Intents[name]
	}
	if p.providerVersion != "" && disk.ProviderVersions[p.providerName] == "" {
		disk.ProviderVersions[p.providerName] = p.providerVersion
	}
	changed, err := lock.Save(a.e.BaseLockPath, disk)
	if err != nil {
		return kindErr(KindStateStore, "state or lock write failed", fmt.Errorf("save base lock: %w", err))
	}
	for _, it := range p.intents {
		if p.baseWrite[it.Name] {
			if err := a.e.writeBaseText(it.Name, it.Body); err != nil {
				return kindErr(KindStateStore, "state or lock write failed", err)
			}
		}
	}
	p.baseWrite = nil
	p.Written = p.Written || changed
	return nil
}

// setLockResource replaces the entry with r's key, or appends r.
func setLockResource(li *lock.Intent, r lock.Resource) {
	for i := range li.Resources {
		if li.Resources[i].Key == r.Key {
			li.Resources[i] = r
			return
		}
	}
	li.Resources = append(li.Resources, r)
}

// idOf returns the string "id" attribute of a state in cty JSON, "" if none.
func idOf(stateJSON []byte) string {
	var m struct {
		ID any `json:"id"`
	}
	if json.Unmarshal(stateJSON, &m) != nil {
		return ""
	}
	id, _ := m.ID.(string)
	return id
}
