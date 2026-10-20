package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/provider"
	"github.com/giammbo/prosa/internal/state"
)

// twoThingsProject is a project with one intent of two things, a and b,
// and its fixtures.
func twoThingsProject(t *testing.T) (dir, fx string) {
	t.Helper()
	root := t.TempDir()
	dir = projectAt(t, root)
	text := "two things"
	writeFile(t, filepath.Join(dir, "app.md"), text)
	fx = filepath.Join(root, "fx.json")
	writeFile(t, fx, `[{"intent": "app", "hash": "`+sha256Of(text)+`", "resources": [
		{"key": "a", "type": "test_thing", "config": {"name": "a"}},
		{"key": "b", "type": "test_thing", "config": {"name": "b"}}]}]`)
	t.Setenv("PROSA_STATE_PASSPHRASE", "passphrase")
	return dir, fx
}

// interruptingProvider cancels the command's context in its first apply,
// like a Ctrl-C while it runs; a call whose context is cancelled fails, as
// a gRPC call does.
type interruptingProvider struct {
	*fakeProvider
	cancel func()
}

func (p interruptingProvider) ApplyResource(ctx context.Context, typeName string, prior cty.Value, change provider.PlannedChange, config cty.Value) (provider.ApplyResult, error) {
	p.cancel()
	if ctx.Err() != nil {
		return provider.ApplyResult{}, errors.New("rpc error: code = Canceled")
	}
	return p.fakeProvider.ApplyResource(ctx, typeName, prior, change, config)
}

func (p interruptingProvider) source(engine.AWSSettings) (providerFunc, func(), error) {
	return func(context.Context, string, string) (engine.Provider, error) { return p, nil }, func() {}, nil
}

func TestApplyInterruptedSaysHowFarItGot(t *testing.T) {
	dir, fx := twoThingsProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	code := reconcile(ctx, "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, interruptingProvider{fake, cancel}.source, noPlanner)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if want := "prosa: interrupted: finished 1 of 2 operations; run plan again\n"; afterTarget(t, stderr.String()) != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
	if fake.applies != 1 {
		t.Fatalf("applies = %d, want 1", fake.applies)
	}
	evs := readEvents(t, filepath.Join(dir, ".prosa", "events.jsonl"))
	if got, want := eventTypes(evs), []string{"run.start", "plan", "approval", "apply.op", "run.end"}; !eqStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if end := eventData(evs[4]); end["result"] != "interrupted" || end["error_kind"] != engine.KindInterrupted {
		t.Fatalf("run.end = %v", end)
	}
}

func TestRecoverCommand(t *testing.T) {
	dir, fx := twoThingsProject(t)
	root := dir
	fake := &fakeProvider{}
	recoverCmd := func() (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"recover", dir}, strings.NewReader(""), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}
	var stdout, stderr bytes.Buffer
	if code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("apply: exit %d (%s)", code, stderr.String())
	}

	if code, out, errOut := recoverCmd(); code != 0 || out != "nothing to recover\n" {
		t.Fatalf("recover on a consistent project: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	evs := readEvents(t, filepath.Join(root, ".prosa", "events.jsonl"))
	if got := eventTypes(evs[len(evs)-2:]); !eqStrings(got, []string{"run.start", "run.end"}) {
		t.Fatalf("recover events = %v", eventTypes(evs))
	}
	if start, end := eventData(evs[len(evs)-2]), eventData(evs[len(evs)-1]); start["command"] != "recover" || end["result"] != "ok" {
		t.Fatalf("recover events = %v, %v", start, end)
	}

	// The state is saved again but the lock is not, as after a crash.
	store, err := state.Open(filepath.Join(root, ".prosa", "state"), "passphrase")
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := store.Load("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save("app", st); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := reconcile(context.Background(), "plan", []string{"--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 1 ||
		!strings.Contains(stderr.String(), `run "prosa recover <dir>"`) {
		t.Fatalf("plan with the state ahead: exit %d, stderr %q", code, stderr.String())
	}
	want := "recovered app: 2 resource(s) from state serial " + strconv.FormatInt(st.Serial, 10) + "; run plan again\n"
	if code, out, errOut := recoverCmd(); code != 0 || out != want {
		t.Fatalf("recover: exit %d, stdout %q (want %q), stderr %q", code, out, want, errOut)
	}
	stdout.Reset()
	stderr.Reset()
	if code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, fake.source, noPlanner); code != 0 {
		t.Fatalf("apply after recover: exit %d (%s)", code, stderr.String())
	}
}

func TestRecoverUsage(t *testing.T) {
	for _, args := range [][]string{{"recover", "a", "b"}, {"recover", "--fixtures", "fx.json", "d"}, {"recover", "--yes", "d"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "prosa recover") {
			t.Errorf("run %v: exit %d, stderr %q; want 2 and the usage", args, code, stderr.String())
		}
	}
}

// The first signal cancels the context, writing the notice of that moment;
// the second one exits at once.
func TestInterruptibleSecondSignalExits(t *testing.T) {
	for _, notice := range []string{"prosa: stopping after app/a (create) finishes; press Ctrl-C again to force (may leave a resource unrecorded)", "prosa: interrupted"} {
		var stderr bytes.Buffer
		exited := make(chan struct{})
		ctx, stop := interruptible(context.Background(), func() string { return notice }, &stderr, func() { close(exited) }, syscall.SIGUSR1)

		if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the first signal did not cancel the context")
		}
		select {
		case <-exited:
			t.Fatal("exited on the first signal")
		case <-time.After(100 * time.Millisecond):
		}
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("the second signal did not exit")
		}
		stop()
		if want := notice + "\n"; stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	}
}

// The notice names the provider operation in flight; with none (planning,
// the approval prompt, check, recover) it just says the command stops.
func TestInterruptNoticeNamesOperationInFlight(t *testing.T) {
	for current, want := range map[string]string{
		"app/db (replace)": "prosa: stopping after app/db (replace) finishes; press Ctrl-C again to force (may leave a resource unrecorded)",
		"":                 "prosa: interrupted",
	} {
		if got := interruptNotice(current); got != want {
			t.Errorf("interruptNotice(%q) = %q, want %q", current, got, want)
		}
	}
}

// inFlightProvider records, in each apply, the operation the CLI would name
// at a Ctrl-C.
type inFlightProvider struct {
	*fakeProvider
	seen *[]string
}

func (p inFlightProvider) ApplyResource(ctx context.Context, typeName string, prior cty.Value, change provider.PlannedChange, config cty.Value) (provider.ApplyResult, error) {
	*p.seen = append(*p.seen, inFlight.Current())
	return p.fakeProvider.ApplyResource(ctx, typeName, prior, change, config)
}

func (p inFlightProvider) source(engine.AWSSettings) (providerFunc, func(), error) {
	return func(context.Context, string, string) (engine.Provider, error) { return p, nil }, func() {}, nil
}

func TestApplyExposesOperationInFlight(t *testing.T) {
	dir, fx := twoThingsProject(t)
	var seen []string
	var stdout, stderr bytes.Buffer
	code := reconcile(context.Background(), "apply", []string{"--yes", "--fixtures", fx, dir}, strings.NewReader(""), &stdout, &stderr, inFlightProvider{&fakeProvider{}, &seen}.source, noPlanner)
	if code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, stderr.String())
	}
	if want := []string{"app/a (create)", "app/b (create)"}; !eqStrings(seen, want) {
		t.Errorf("in flight during the applies = %v, want %v", seen, want)
	}
	if cur := inFlight.Current(); cur != "" {
		t.Errorf("in flight after apply = %q, want none", cur)
	}
}

// A Ctrl-C at the approval prompt does not wait for stdin: nothing is
// applied and the command ends.
func TestApprovalPromptStopsOnInterrupt(t *testing.T) {
	dir, fx := twoThingsProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin, w := io.Pipe() // nobody ever answers
	defer w.Close()
	fake := &fakeProvider{}
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- reconcile(ctx, "apply", []string{"--fixtures", fx, dir}, stdin, &stdout, &stderr, fake.source, noPlanner)
	}()
	time.Sleep(200 * time.Millisecond) // at the prompt
	cancel()
	select {
	case code := <-done:
		if code != 1 || fake.applies != 0 {
			t.Fatalf("exit %d, applies %d, want 1 and 0 (stderr %q)", code, fake.applies, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the approval prompt kept waiting after the interrupt")
	}
	if _, err := os.Stat(filepath.Join(dir, "prosa.lock.yaml")); !os.IsNotExist(err) {
		t.Errorf("lock written after an interrupted prompt (stat err %v)", err)
	}
}
