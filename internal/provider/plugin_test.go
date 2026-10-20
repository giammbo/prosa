package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartFailsOnNonPlugin(t *testing.T) {
	t.Setenv("PROSA_LOG", "off") // go-plugin warns about the non-plugin exiting
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	p, err := Start(ctx, "/bin/echo")
	if err == nil {
		p.Close()
		t.Fatal("expected error starting a non-plugin binary")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Start took %s", d)
	}
}

func TestCloseIdempotent(t *testing.T) {
	var p *Provider
	p.Close() // nil receiver is safe
	p = &Provider{}
	p.Close()
	p.Close()
}

// TestStartCancelledContextKillsProcess starts a fake plugin that never
// completes the handshake, cancels the context mid-start, and checks that
// Start returns well before the handshake timeout and that the child process
// is gone (not orphaned).
func TestStartCancelledContextKillsProcess(t *testing.T) {
	t.Setenv("PROSA_LOG", "off") // the killed plugin is logged as an error
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := filepath.Join(dir, "terraform-provider-hang")
	body := fmt.Sprintf("#!/bin/sh\necho $$ > %q\nexec sleep 30\n", pidFile)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // cancel once the child is running
		for ctx.Err() == nil {
			if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 && b[len(b)-1] == '\n' {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	p, err := Start(ctx, script)
	if err == nil {
		p.Close()
		t.Fatal("expected an error from a cancelled start")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > startTimeout/2 {
		t.Errorf("Start took %s after cancellation, want well under the %s handshake timeout", d, startTimeout)
	}

	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake plugin never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil { // ESRCH once the process is gone and reaped
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("plugin process %d still running after Start returned", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
