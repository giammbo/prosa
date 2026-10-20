package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giammbo/prosa/internal/engine"
)

// fakeClock is a clock that moves only when Advance is called.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, fakeWaiter{c.now.Add(d), ch})
	return ch
}

// Advance moves the clock by d and fires the waiters that are due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// waitWaiters waits until n waiters are pending: the ticker of the printer
// is then blocked on the clock, after printing what it had to.
func (c *fakeClock) waitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.waiters)
		c.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func op(phase engine.OpPhase, action engine.Action, node, id string, err error) engine.OpProgress {
	intent, key, _ := cutNode(node)
	return engine.OpProgress{Intent: intent, Key: key, Action: action, Phase: phase, ID: id, Err: err}
}

func cutNode(node string) (string, string, bool) {
	for i := range node {
		if node[i] == '/' {
			return node[:i], node[i+1:], true
		}
	}
	return node, "", false
}

func TestProgressLines(t *testing.T) {
	clk := newFakeClock()
	var out, errOut bytes.Buffer
	p := newProgressPrinter(&out, &errOut, clk, false)

	p.handle(op(engine.OpStart, engine.Create, "postgres/db.instance", "", nil))
	for _, d := range []time.Duration{10 * time.Second, 10 * time.Second, 52 * time.Second} {
		clk.waitWaiters(t, 1)
		clk.Advance(d)
	}
	clk.waitWaiters(t, 1)
	p.handle(op(engine.OpDone, engine.Create, "postgres/db.instance", "staging-db", nil))

	p.handle(op(engine.OpStart, engine.Update, "web-app/app.service", "svc-1", nil))
	clk.waitWaiters(t, 2) // the create's last tick is still pending
	clk.Advance(10 * time.Second)
	clk.waitWaiters(t, 1)
	clk.Advance(3 * time.Second)
	p.handle(op(engine.OpDone, engine.Update, "web-app/app.service", "svc-1", nil))

	p.handle(op(engine.OpStart, engine.Delete, "cron/job.schedule", "staging-cron", nil))
	clk.Advance(5 * time.Second)
	p.handle(op(engine.OpDone, engine.Delete, "cron/job.schedule", "", nil))

	want := "postgres/db.instance: Creating...\n" +
		"postgres/db.instance: Still creating... [10s elapsed]\n" +
		"postgres/db.instance: Still creating... [20s elapsed]\n" +
		"postgres/db.instance: Still creating... [1m12s elapsed]\n" +
		"postgres/db.instance: Creation complete after 1m12s [id=staging-db]\n" +
		"web-app/app.service: Modifying... [id=svc-1]\n" +
		"web-app/app.service: Still modifying... [id=svc-1, 10s elapsed]\n" +
		"web-app/app.service: Modifications complete after 13s [id=svc-1]\n" +
		"cron/job.schedule: Destroying... [id=staging-cron]\n" +
		"cron/job.schedule: Destruction complete after 5s\n"
	if out.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q", errOut.String())
	}
	if p.added != 1 || p.changed != 1 || p.destroyed != 1 {
		t.Errorf("counts = %d added, %d changed, %d destroyed; want 1, 1, 1", p.added, p.changed, p.destroyed)
	}
}

func TestProgressFailure(t *testing.T) {
	clk := newFakeClock()
	var out, errOut bytes.Buffer
	p := newProgressPrinter(&out, &errOut, clk, false)
	boom := errors.New("app/db: quota exceeded")
	p.handle(op(engine.OpStart, engine.Create, "app/db", "", nil))
	clk.Advance(2 * time.Second)
	p.handle(op(engine.OpFailed, engine.Create, "app/db", "", boom))
	if want := "app/db: Creating...\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
	if want := "Error: app/db: quota exceeded\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
	if !p.reported(boom) || !p.reported(errors.Join(boom)) || p.reported(errors.New("other")) || p.reported(nil) {
		t.Error("reported does not recognize the failure it printed")
	}
	if p.added != 0 {
		t.Errorf("added = %d after a failure", p.added)
	}
}

func TestProgressColor(t *testing.T) {
	var out bytes.Buffer
	p := newProgressPrinter(&out, io.Discard, newFakeClock(), true)
	p.handle(op(engine.OpStart, engine.Create, "app/db", "", nil))
	p.handle(op(engine.OpDone, engine.Create, "app/db", "db-1", nil))
	want := "\x1b[1mapp/db:\x1b[0m Creating...\n\x1b[1mapp/db:\x1b[0m Creation complete after 0s [id=db-1]\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestApplyCompleteLine(t *testing.T) {
	p := &progressPrinter{added: 2, changed: 1, destroyed: 3}
	var out bytes.Buffer
	writeApplyComplete(&out, p, false)
	if want := "Apply complete! Resources: 2 added, 1 changed, 3 destroyed.\n"; out.String() != want {
		t.Errorf("line = %q, want %q", out.String(), want)
	}
	out.Reset()
	writeApplyComplete(&out, p, true)
	if want := "\x1b[1m\x1b[32mApply complete! Resources: 2 added, 1 changed, 3 destroyed.\x1b[0m\n"; out.String() != want {
		t.Errorf("colored line = %q, want %q", out.String(), want)
	}
}

func TestUseColor(t *testing.T) {
	tty := isTerminal
	t.Cleanup(func() { isTerminal = tty })
	isTerminal = func(any) bool { return true }
	t.Setenv("NO_COLOR", "")
	if !useColor(false, io.Discard) {
		t.Error("a terminal without NO_COLOR nor --no-color: no color")
	}
	if useColor(true, io.Discard) {
		t.Error("--no-color: color")
	}
	t.Setenv("NO_COLOR", "1")
	if useColor(false, io.Discard) {
		t.Error("NO_COLOR=1: color")
	}
	t.Setenv("NO_COLOR", "")
	isTerminal = tty
	if useColor(false, &bytes.Buffer{}) {
		t.Error("not a terminal: color")
	}
}

func TestNoColorFlag(t *testing.T) {
	dir := projectAt(t, t.TempDir())
	for _, cmd := range []string{"plan", "apply", "check", "recover"} {
		var stderr bytes.Buffer
		f, err := parseReconcileFlags(cmd, []string{"--no-color", dir}, &stderr)
		if err != nil || !f.noColor {
			t.Errorf("%s --no-color: err %v, noColor %v (stderr %q)", cmd, err, f.noColor, stderr.String())
		}
	}
}

func TestApplyFailurePrintedOnce(t *testing.T) {
	dir, fx := gatedProject(t, "---\nautonomy: auto\n---\none thing")
	fake := &fakeProvider{failApply: errors.New("quota exceeded")}
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if got, want := afterTarget(t, stderr.String()), "Error: app/thing: quota exceeded\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if !strings.HasSuffix(stdout.String(), "app/thing: Creating...\n") || strings.Contains(stdout.String(), "Apply complete!") {
		t.Errorf("stdout = %q", stdout.String())
	}
}
