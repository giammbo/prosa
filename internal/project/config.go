// Package project opens a Prosa project (ADR-029): a folder whose *.md are
// the intents, the lock in prosa.lock.yaml and the engine's own files in
// .prosa/. There is no configuration file: credentials and region come from
// the AWS CLI's chain.
package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/giammbo/prosa/internal/intent"
)

// legacyFile is the configuration file of ADR-027, which no longer exists.
const legacyFile = "prosa.yaml"

// ErrLegacyConfig is the error of Open in a folder that still has a
// prosa.yaml.
var ErrLegacyConfig = errors.New(legacyFile + " is no longer used (ADR-029): credentials and region come from the AWS CLI; delete it")

// Project is a folder of intents.
type Project struct {
	Root string // absolute
}

// Open returns the project at root. It refuses a folder with a prosa.yaml,
// returning ErrLegacyConfig: whoever wrote one
// expected the guard on the account it declared, so it is never ignored in
// silence.
func Open(root string) (*Project, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, legacyFile)); err == nil {
		return nil, ErrLegacyConfig
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return &Project{Root: abs}, nil
}

// LockPath is the lock of the project.
func (p *Project) LockPath() string { return filepath.Join(p.Root, "prosa.lock.yaml") }

// StateDir is the state store of the project.
func (p *Project) StateDir() string { return filepath.Join(p.Root, ".prosa", "state") }

// EventsPath is the event log of the project.
func (p *Project) EventsPath() string { return filepath.Join(p.Root, ".prosa", "events.jsonl") }

// PlannerCacheDir is the planner cache of the project.
func (p *Project) PlannerCacheDir() string { return filepath.Join(p.Root, ".prosa", "planner") }

// BaseTextDir keeps the text each intent's base was planned from (ADR-030).
func (p *Project) BaseTextDir() string { return filepath.Join(p.Root, ".prosa", "base") }

// EnvPaths are the lock, the state store and the event log of the
// environment env: the lock sits next to the environment file, the others in
// the project's .prosa/ (ADR-029). The planner cache is shared.
func (p *Project) EnvPaths(env intent.Env) (lockPath, stateDir, eventsPath string) {
	lockPath = filepath.Join(filepath.Dir(env.Path), env.Name+".lock.yaml")
	stateDir = filepath.Join(p.Root, ".prosa", "state.d", env.Name)
	eventsPath = filepath.Join(p.Root, ".prosa", "events", env.Name+".jsonl")
	return
}

// CheckEnvLocation refuses an environment file in the project root, where
// every .md is an intent.
func (p *Project) CheckEnvLocation(env intent.Env) error {
	if filepath.Dir(env.Path) == p.Root {
		return fmt.Errorf("%s is in the project root, where every .md is an intent: move it to envs/", filepath.Base(env.Path))
	}
	return nil
}
