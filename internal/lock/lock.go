// Package lock reads and writes the lock, prosa.lock.yaml: the frozen concrete choices (ADR-010, ADR-016).
package lock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const header = "# Generato da prosa. Non modificare a mano (ADR-016).\n"

type Lock struct {
	Version          int                `yaml:"version"`
	ProviderVersions map[string]string  `yaml:"provider_versions"`
	Intents          map[string]*Intent `yaml:"intents"`
}

type Intent struct {
	IntentHash string `yaml:"intent_hash"`
	// BaseHash and Patch are those of an environment lock (ADR-030): the
	// intent_hash of the base this intent derives from, and the operations
	// applied to it.
	BaseHash     string     `yaml:"base_hash,omitempty"`
	Patch        []PatchOp  `yaml:"patch,omitempty"`
	Recipe       *string    `yaml:"recipe"`
	DependsOn    []string   `yaml:"depends_on,omitempty"`
	ReconciledAt time.Time  `yaml:"reconciled_at"`
	State        StateRef   `yaml:"state"`
	Resources    []Resource `yaml:"resources"`
	Checks       []Check    `yaml:"checks,omitempty"`
}

type StateRef struct {
	Serial int64  `yaml:"serial"`
	Digest string `yaml:"digest"`
}

type Resource struct {
	Key      string         `yaml:"key"`
	Provider string         `yaml:"provider"`
	Type     string         `yaml:"type"`
	ID       string         `yaml:"id"`
	Chosen   map[string]any `yaml:"chosen"`
	// DependsOn lists the nodes ("<intent>/<key>") created before this
	// resource without a reference to them (ADR-020).
	DependsOn []string `yaml:"depends_on,omitempty"`
}

// Check is the definition of a check compiled from the intent's prose
// (ADR-021): Name is the original sentence, Type one of resource_attr,
// http_get, tls_days and unverifiable, the other fields those of its type.
// Results are never stored here: they go to the event log. The JSON names
// are those of the planner's submit_desired_state tool.
type Check struct {
	Name     string `yaml:"name" json:"name"`
	Type     string `yaml:"type" json:"type"`
	Resource string `yaml:"resource,omitempty" json:"resource,omitempty"`
	Path     string `yaml:"path,omitempty" json:"path,omitempty"`
	Op       string `yaml:"op,omitempty" json:"op,omitempty"`
	// Value is a plain YAML/JSON value: string, bool, number, list or map.
	Value   any    `yaml:"value,omitempty" json:"value,omitempty"`
	URL     string `yaml:"url,omitempty" json:"url,omitempty"`
	Status  int    `yaml:"status,omitempty" json:"status,omitempty"`
	Within  string `yaml:"within,omitempty" json:"within,omitempty"`
	Host    string `yaml:"host,omitempty" json:"host,omitempty"`
	MinDays int    `yaml:"min_days,omitempty" json:"min_days,omitempty"`
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

func empty() *Lock {
	return &Lock{Version: 1, ProviderVersions: map[string]string{}, Intents: map[string]*Intent{}}
}

// Load reads the lock; a missing file yields an empty lock.
func Load(path string) (*Lock, error) {
	l, _, err := Read(path)
	return l, err
}

// Read is Load that also returns the bytes it parsed, nil when the file does
// not exist. Loading is strict: unknown fields, a version other than 1, an
// intent without an entry and a resource key repeated within an intent are
// errors naming the file.
func Read(path string) (*Lock, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty(), nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	l := &Lock{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(l); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, fmt.Errorf("%s: empty lock file", path)
		}
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Version != 1 {
		return nil, nil, fmt.Errorf("%s: unsupported lock version %d (want 1)", path, l.Version)
	}
	if l.ProviderVersions == nil {
		l.ProviderVersions = map[string]string{}
	}
	if l.Intents == nil {
		l.Intents = map[string]*Intent{}
	}
	for _, name := range slices.Sorted(maps.Keys(l.Intents)) {
		in := l.Intents[name]
		if in == nil {
			return nil, nil, fmt.Errorf("%s: intent %q has an empty entry", path, name)
		}
		seen := map[string]bool{}
		for _, r := range in.Resources {
			if seen[r.Key] {
				return nil, nil, fmt.Errorf("%s: intent %q: duplicate resource key %q", path, name, r.Key)
			}
			seen[r.Key] = true
		}
	}
	return l, data, nil
}

// Marshal renders the lock deterministically (sorted resources, sorted map keys, UTC times).
// The receiver is not modified.
func (l *Lock) Marshal() ([]byte, error) {
	c := *l
	c.Intents = make(map[string]*Intent, len(l.Intents))
	for name, in := range l.Intents {
		ci := *in
		ci.ReconciledAt = in.ReconciledAt.UTC()
		ci.Resources = make([]Resource, len(in.Resources))
		for i, r := range in.Resources {
			where := name + "/" + r.Key
			n, err := yamlNumbers(r.Chosen, where)
			if err != nil {
				return nil, err
			}
			r.Chosen, _ = n.(map[string]any)
			ci.Resources[i] = r
		}
		sort.SliceStable(ci.Resources, func(i, j int) bool { return ci.Resources[i].Key < ci.Resources[j].Key })
		ci.Checks = make([]Check, len(in.Checks))
		for i, ck := range in.Checks {
			v, err := yamlNumbers(ck.Value, fmt.Sprintf("%s: check %q", name, ck.Name))
			if err != nil {
				return nil, err
			}
			ck.Value = v
			ci.Checks[i] = ck
		}
		if len(in.Patch) > 0 {
			ci.Patch = make([]PatchOp, len(in.Patch))
			for i, op := range in.Patch {
				where := fmt.Sprintf("%s: patch op %d", name, i)
				v, err := yamlNumbers(op.Value, where)
				if err != nil {
					return nil, err
				}
				op.Value = v
				cfg, err := yamlNumbers(op.Config, where)
				if err != nil {
					return nil, err
				}
				op.Config, _ = cfg.(map[string]any)
				ci.Patch[i] = op
			}
		}
		c.Intents[name] = &ci
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes the lock only if the bytes differ from the file on disk.
func Save(path string, l *Lock) (bool, error) {
	data, err := l.Marshal()
	if err != nil {
		return false, err
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lock-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

// jsonNumber is the JSON number grammar: no hex, NaN, Inf or leading zeros.
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// integer matches a JSON number without fraction or exponent.
var integer = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// CheckNumber reports whether n can be written to the lock and read back as
// the same number: JSON syntax, integers within int64/uint64, other numbers
// within float64 without overflow or underflow of a non-zero literal.
func CheckNumber(n json.Number) error {
	s := n.String()
	bad := fmt.Errorf("number %s out of range", s)
	if !jsonNumber.MatchString(s) {
		return bad
	}
	if integer.MatchString(s) {
		_, errI := strconv.ParseInt(s, 10, 64)
		_, errU := strconv.ParseUint(s, 10, 64)
		if errI != nil && errU != nil {
			return bad
		}
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return bad
	}
	if f == 0 {
		mantissa, _, _ := strings.Cut(strings.ToLower(s), "e")
		if strings.ContainsAny(mantissa, "123456789") {
			return bad
		}
	}
	return nil
}

// yamlNumbers returns a copy of v where every json.Number (a planner value
// decoded with UseNumber) is a YAML number node with the same digits, which
// the YAML encoder would otherwise write as a quoted string. A number that
// does not fit int64/uint64 (integers) or float64 (the rest) is an error
// naming where, since YAML readers would not give it back unchanged. A nil
// map stays nil.
func yamlNumbers(v any, where string) (any, error) {
	switch t := v.(type) {
	case json.Number:
		if err := CheckNumber(t); err != nil {
			return nil, fmt.Errorf("%w for %q", err, where)
		}
		tag := "!!float"
		if integer.MatchString(t.String()) {
			tag = "!!int"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: t.String()}, nil
	case map[string]any:
		if t == nil {
			return t, nil
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			ne, err := yamlNumbers(e, where+"."+k)
			if err != nil {
				return nil, err
			}
			out[k] = ne
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			ne, err := yamlNumbers(e, fmt.Sprintf("%s[%d]", where, i))
			if err != nil {
				return nil, err
			}
			out[i] = ne
		}
		return out, nil
	}
	return v, nil
}
