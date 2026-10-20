package project

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// gitignoreEntry keeps the engine's files (state, planner cache, events)
// out of the repository.
const gitignoreEntry = ".prosa/"

// Init makes root (creating it if needed) a project: it makes .gitignore
// exclude .prosa/, creating it or appending the line, and creates the envs/
// folder for the environment files. What is already there is left as it is.
// It returns what it did, one line per file.
func Init(root string) ([]string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	var done []string
	line, err := ignoreEngineFiles(filepath.Join(root, ".gitignore"))
	if err != nil {
		return done, err
	}
	done = append(done, line)
	envs := filepath.Join(root, "envs")
	switch _, err := os.Stat(envs); {
	case err == nil:
		done = append(done, "kept envs/")
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(envs, 0o755); err != nil {
			return done, err
		}
		done = append(done, "created envs/")
	default:
		return done, err
	}
	return done, nil
}

// ignoreEngineFiles makes the .gitignore at path exclude .prosa/ and says
// what it did; nothing ("") when it already does.
func ignoreEngineFiles(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "created .gitignore", os.WriteFile(path, []byte(gitignoreEntry+"\n"), 0o644)
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if slices.ContainsFunc(lines, func(l string) bool {
		l = strings.TrimSpace(l)
		return l == ".prosa" || l == ".prosa/" || l == "/.prosa" || l == "/.prosa/" || l == ".prosa/*" || l == ".prosa/**"
	}) {
		return "kept .gitignore", nil
	}
	add := gitignoreEntry + "\n"
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		add = "\n" + add
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(add)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return "added " + gitignoreEntry + " to .gitignore", err
}
