package provider

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-hclog"
)

// Logging of a provider process. go-plugin logs its own events and, through
// a sub-logger named after the plugin binary, every line the provider writes
// on stderr. The two get separate levels: Prosa's (default warn) and the
// provider's (default error, so its routine warnings stay quiet). PROSA_LOG
// sets both. Provider logs only go to Prosa's stderr, never to a file.

// newLogger returns the go-plugin logger for the plugin binary at path,
// writing to out.
func newLogger(out io.Writer, path string) hclog.Logger {
	level, provLevel := hclog.Warn, hclog.Error
	if v := os.Getenv("PROSA_LOG"); v != "" {
		if l := hclog.LevelFromString(v); l != hclog.NoLevel {
			level, provLevel = l, l
		}
	}
	return &pluginLogger{
		Logger:     hclog.New(&hclog.LoggerOptions{Name: "prosa-plugin", Level: level, Output: out, IndependentLevels: true}),
		binary:     filepath.Base(path),
		providerLv: provLevel,
	}
}

// pluginLogger gives the sub-logger named after the plugin binary, which
// go-plugin uses for the provider's own lines, the provider level.
type pluginLogger struct {
	hclog.Logger
	binary     string
	providerLv hclog.Level
}

func (l *pluginLogger) Named(name string) hclog.Logger {
	sub := l.Logger.Named(name)
	if name == l.binary {
		sub.SetLevel(l.providerLv) // IndependentLevels: the parent keeps its own
	}
	return sub
}

// noDiskLogVars are the variables that make a provider write its logs to a
// file (terraform-plugin-sdk and terraform-plugin-go); they never reach it.
var noDiskLogVars = []string{"TF_LOG_PATH", "TF_LOG_PATH_MASK"}

// pluginEnv is the environment of a provider process: host without the
// variables that would write logs to disk, then extra ("KEY=VALUE"), whose
// keys replace the host's. An extra entry without "=" only removes that key.
func pluginEnv(host, extra []string) []string {
	drop := map[string]bool{}
	for _, k := range noDiskLogVars {
		drop[k] = true
	}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		drop[k] = true
	}
	var out []string
	for _, kv := range host {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	for _, kv := range extra {
		if strings.Contains(kv, "=") {
			out = append(out, kv)
		}
	}
	return out
}
