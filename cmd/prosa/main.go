// Command prosa is the command-line entry point of Prosa.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hashicorp/go-plugin"

	"github.com/giammbo/prosa/internal/engine"
)

func main() {
	os.Exit(mainCode()) // os.Exit skips defers: they live in mainCode
}

// mainCode runs the command with a context cancelled by Ctrl-C or SIGTERM,
// so downloads abort (removing their staging directory), an apply finishes
// the operation in progress and stops (ADR-024), and providers are stopped
// instead of being orphaned; CleanupClients kills any provider process still
// running on the way out. The first signal says what the command waits for
// (see interruptNotice); a second one exits at once, with code 130.
func mainCode() int {
	ctx, stop := interruptible(context.Background(), func() string { return interruptNotice(inFlight.Current()) }, os.Stderr, func() {
		plugin.CleanupClients()
		os.Exit(130)
	}, os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer plugin.CleanupClients()
	return run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// inFlight holds the provider apply in progress of the command's apply,
// for the notice of the first signal.
var inFlight = &engine.InFlight{}

// interruptNotice is what the first signal writes: with an operation in
// flight (current, "<intent>/<key> (<action>)"), that the command stops
// after it (ADR-024); otherwise (planning, the approval prompt, check,
// recover) only that it is interrupted.
func interruptNotice(current string) string {
	if current == "" {
		return "prosa: interrupted"
	}
	return "prosa: stopping after " + current + " finishes; press Ctrl-C again to force (may leave a resource unrecorded)"
}

// interruptible returns a context cancelled by the first of sigs, at which
// it writes notice() on stderr; the second one, received on its own
// channel, calls hardExit. stop releases the signals.
func interruptible(parent context.Context, notice func() string, stderr io.Writer, hardExit func(), sigs ...os.Signal) (ctx context.Context, stop func()) {
	ctx, stopCtx := signal.NotifyContext(parent, sigs...)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	go func() {
		for n := 1; ; n++ {
			select {
			case <-ch:
			case <-done:
				return
			}
			if n > 1 {
				hardExit()
				return
			}
			fmt.Fprintln(stderr, notice())
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		close(done)
		stopCtx()
	}
}

// run dispatches the subcommand and returns the process exit code:
// 0 on success, 1 on runtime errors, 2 on usage errors.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "prosa %s\n", version)
		return 0
	case "init":
		return initCommand(args[1:], stdout, stderr)
	case "plan", "apply":
		return reconcile(ctx, args[0], args[1:], stdin, stdout, stderr, poolProviders, llmPlanner)
	case "check":
		return checkCommand(ctx, args[1:], stdout, stderr, poolProviders)
	case "recover":
		return recoverCommand(args[1:], stdout, stderr)
	case "debug":
		return runDebug(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "prosa: unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  prosa version
  prosa init [<dir>]
  prosa plan [--env FILE] [--ignore FILES] [--lock FILE] [--state DIR] [--fixtures FILE | --model NAME [--patch-model NAME]] [--no-cache] [--events FILE | --no-events] [--require-endpoint] [--no-color] [<dir>]
  prosa apply [--yes] [--env FILE] [--ignore FILES] [--lock FILE] [--state DIR] [--fixtures FILE | --model NAME [--patch-model NAME]] [--no-cache] [--events FILE | --no-events] [--require-endpoint] [--no-color] [<dir>]
  prosa check [--env FILE] [--ignore FILES] [--lock FILE] [--state DIR] [--events FILE | --no-events] [--require-endpoint] [--no-color] [<dir>]
  prosa recover [--env FILE] [--ignore FILES] [--lock FILE] [--state DIR] [--events FILE | --no-events] [--require-endpoint] [--no-color] [<dir>]
  prosa debug schema <namespace/name>[@version] <resource_type>

<dir> is the project: a folder whose *.md files are the intents (except the
--ignore ones, README.md by default); the current directory by default.
prosa init prepares it. The lock is <dir>/prosa.lock.yaml, the engine's files
are in <dir>/.prosa/. Credentials and region come from the AWS CLI's chain
(AWS_REGION, or the region of AWS_PROFILE); PROSA_AWS_REGION overrides it.

--env FILE is an environment (ADR-029): a prose file, usually envs/<name>.md and
never in <dir> itself, that wins over what the intents say. Its lock is
<name>.lock.yaml next to it, its state is <dir>/.prosa/state.d/<name>/ and its
events <dir>/.prosa/events/<name>.jsonl; --lock, --state and --events win.

The planner's LLM is chosen by PROSA_LLM: anthropic, the default when its
credentials are there (ANTHROPIC_API_KEY or ant auth login), or openai, an
OpenAI-compatible API at PROSA_LLM_BASE_URL (PROSA_LLM_API_KEY when it wants
one). --model (PROSA_MODEL) plans, --patch-model (PROSA_PATCH_MODEL) writes
patches; PROSA_EFFORT and PROSA_PATCH_EFFORT are low, medium or high (high and
low by default). PROSA_SCHEMA_BYTES caps a schema shown to the model (30720).

plan, apply, check and recover first say on stderr where they point: the
emulated endpoint of PROSA_AWS_ENDPOINT, or the AWS account of the current
credentials. Their output is colored on a
terminal, unless --no-color or NO_COLOR.
`)
}
