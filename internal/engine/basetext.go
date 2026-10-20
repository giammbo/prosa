package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Base texts (ADR-030): the text each intent's base was planned from, kept
// in Engine.BaseTextDir as <intent>.md next to (in time) the base lock entry
// it produced. It is local, like the rest of .prosa/.

// baseTextPath is the file of the base text of intent name.
func (e *Engine) baseTextPath(name string) string {
	return filepath.Join(e.BaseTextDir, name+".md")
}

// readBaseText returns the base text of intent name and whether there is
// one; none without a BaseTextDir.
func (e *Engine) readBaseText(name string) (string, bool, error) {
	if e.BaseTextDir == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(e.baseTextPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("base text of %s: %w", name, err)
	}
	return string(data), true, nil
}

// writeBaseText keeps body as the base text of intent name; nothing without
// a BaseTextDir.
func (e *Engine) writeBaseText(name, body string) error {
	if e.BaseTextDir == "" {
		return nil
	}
	if err := os.MkdirAll(e.BaseTextDir, 0o755); err != nil {
		return fmt.Errorf("base text of %s: %w", name, err)
	}
	if err := writeAtomic(e.baseTextPath(name), []byte(body)); err != nil {
		return fmt.Errorf("base text of %s: %w", name, err)
	}
	return nil
}

// removeBaseText forgets the base text of intent name.
func (e *Engine) removeBaseText(name string) error {
	if e.BaseTextDir == "" {
		return nil
	}
	if err := os.Remove(e.baseTextPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("base text of %s: %w", name, err)
	}
	return nil
}

// writeAtomic writes data to path through a temporary file in its directory.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
