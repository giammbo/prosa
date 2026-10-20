// Package state is the encrypted store for the provider state of each intent.
package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ResourceState is what the engine keeps per resource. State and Identity are
// cty JSON (ctyjson.Marshal); Identity is empty when absent. Chosen,
// DependsOn and ID are those of the lock entry written with the record, so
// that the lock can be rebuilt from the store (ADR-024); records written
// before they existed have none.
type ResourceState struct {
	Provider        string          `json:"provider"`
	Type            string          `json:"type"`
	SchemaVersion   int64           `json:"schema_version"`
	State           json.RawMessage `json:"state"`
	Private         []byte          `json:"private,omitempty"`
	Identity        json.RawMessage `json:"identity,omitempty"`
	IdentityVersion int64           `json:"identity_version,omitempty"`
	// Chosen is the lock's chosen configuration; its numbers load as
	// json.Number, unchanged.
	Chosen    map[string]any `json:"chosen,omitempty"`
	DependsOn []string       `json:"depends_on,omitempty"`
	ID        string         `json:"id,omitempty"`
}

// IntentState is the state of one intent: resources keyed by resource key.
type IntentState struct {
	Serial    int64                     `json:"serial"`
	Resources map[string]*ResourceState `json:"resources"`
	// Body is the intent's text when it last completed, for the planner.
	Body string `json:"body,omitempty"`
	// EnvBody is the environment file's text when the intent last completed
	// (ADR-029); empty without an environment.
	EnvBody string `json:"env_body,omitempty"`
	// Pending is the desired state an Apply was executing for the intent,
	// changed since the lock, when it stopped before completing it: nil
	// otherwise.
	Pending *Pending `json:"pending,omitempty"`
}

// Pending is the desired state an Apply executes for an intent whose text
// changed (ADR-024): written with the intent's first state save, cleared
// when the intent completes, so that a run resumed after an interruption
// applies what was planned instead of planning again. Desired is the
// engine's JSON of it (resources, depends_on and checks, canonical);
// IntentHash the hash of the text it was planned from.
type Pending struct {
	IntentHash string          `json:"intent_hash"`
	Desired    json.RawMessage `json:"desired"`
}

// Store reads and writes encrypted state files in a directory.
type Store struct {
	dir        string
	passphrase string
}

// Open creates dir (0700) if needed and returns a Store.
func Open(dir, passphrase string) (*Store, error) {
	if passphrase == "" {
		return nil, errors.New("state passphrase is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{dir: dir, passphrase: passphrase}, nil
}

func (s *Store) path(intent, ext string) (string, error) {
	if intent == "" || intent == "." || intent == ".." || strings.ContainsAny(intent, `/\`) || strings.ContainsRune(intent, 0) {
		return "", fmt.Errorf("invalid intent name %q", intent)
	}
	return filepath.Join(s.dir, intent+ext), nil
}

// digestOf is the digest of the deterministic plaintext JSON; it never
// depends on salt or nonce.
func digestOf(plaintext []byte) string {
	sum := sha256.Sum256(plaintext)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Load returns the state of intent and its digest. A missing file yields an
// empty state and digest "".
func (s *Store) Load(intent string) (*IntentState, string, error) {
	p, err := s.path(intent, ".state")
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &IntentState{Resources: map[string]*ResourceState{}}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	pt, err := decrypt(s.passphrase, intent, data)
	if err != nil {
		return nil, "", err
	}
	st := &IntentState{}
	dec := json.NewDecoder(bytes.NewReader(pt))
	dec.UseNumber()
	if err := dec.Decode(st); err != nil {
		return nil, "", fmt.Errorf("parse state for intent %q: %w", intent, err)
	}
	if st.Resources == nil {
		st.Resources = map[string]*ResourceState{}
	}
	return st, digestOf(pt), nil
}

// Save increments st.Serial, encrypts and atomically writes the state, and
// returns its digest.
func (s *Store) Save(intent string, st *IntentState) (string, error) {
	p, err := s.path(intent, ".state")
	if err != nil {
		return "", err
	}
	st.Serial++
	if st.Resources == nil {
		st.Resources = map[string]*ResourceState{}
	}
	pt, err := json.Marshal(st) // struct fields in order, map keys sorted: deterministic
	if err != nil {
		st.Serial--
		return "", err
	}
	enc, err := encrypt(s.passphrase, intent, pt)
	if err != nil {
		st.Serial--
		return "", err
	}
	if err := writeAtomic(s.dir, p, enc); err != nil {
		st.Serial--
		return "", err
	}
	return digestOf(pt), nil
}

func writeAtomic(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove deletes the state file of intent; a missing file is not an error.
func (s *Store) Remove(intent string) error {
	p, err := s.path(intent, ".state")
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Lock takes the exclusive lock of intent: an advisory flock on
// <intent>.lock, held while the file stays open, so the kernel releases it
// when the process dies, even without unlock (a forced exit, ADR-024). The
// file stays on disk. A lock held elsewhere (another process, or another
// Lock in this one) is an error naming the file. unlock is idempotent.
func (s *Store) Lock(intent string) (unlock func(), err error) {
	p, err := s.path(intent, ".lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("state for intent %q is locked (%s)", intent, p)
		}
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	var once sync.Once
	return func() { once.Do(func() { f.Close() }) }, nil // closing releases the lock
}
