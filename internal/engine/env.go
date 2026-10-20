package engine

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/giammbo/prosa/internal/intent"
)

// IntentHash is the hash that decides whether an intent is planned again
// (ADR-029): the intent's own without an environment, so that today's locks
// stay valid; with one, the hash of the intent's, of the environment's common
// lines and of the section of the environment for this intent, so that a
// change to another intent's section does not plan it again.
func IntentHash(it intent.Intent, env *intent.Env) string {
	if env == nil {
		return it.Hash
	}
	sum := sha256.Sum256([]byte(it.Hash + "\n" + shaHex(env.Common()) + "\n" + shaHex(env.Section(it.Name))))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) intentHash(it intent.Intent) string { return IntentHash(it, e.Env) }

func (p *Plan) intentHash(it intent.Intent) string { return IntentHash(it, p.env) }

// envName is the name of the environment, "" without one.
func (p *Plan) envName() string {
	if p.env == nil {
		return ""
	}
	return p.env.Name
}

// envBody is the part of the environment file that concerns the intent
// (common lines and its section), "" without one; it is what the state keeps
// and the planner compares with the next one.
func (p *Plan) envBody(name string) string {
	if p.env == nil {
		return ""
	}
	return intent.EnvFor(*p.env, name)
}

// recoverCommand is the command that rebuilds the lock from the state
// store, as the hints name it.
func (e *Engine) recoverCommand() string {
	if e.Env == nil {
		return "prosa recover <dir>"
	}
	file := e.EnvFile
	if file == "" {
		file = e.Env.Path
	}
	return "prosa recover --env " + file
}
