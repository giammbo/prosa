package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/giammbo/prosa/internal/checks"
	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/events"
	"github.com/giammbo/prosa/internal/intent"
	"github.com/giammbo/prosa/internal/lock"
	"github.com/giammbo/prosa/internal/planner"
	"github.com/giammbo/prosa/internal/project"
	"github.com/giammbo/prosa/internal/registry"
	"github.com/giammbo/prosa/internal/state"
)

// approvePrompt is what apply asks on stdin without --yes, after the list of
// the changes that need an approval.
const approvePrompt = "Apply these changes? Only 'yes' is accepted: "

// reconcileFlags are the flags of plan, apply, check and recover, with
// what the project and the AWS CLI's configuration add (ADR-029).
type reconcileFlags struct {
	dir, lock, state, fixtures string // dir: the project root, absolute
	envFile                    string
	env                        intent.Env // zero without --env
	baseLock                   string     // with --env, the lock of the base: the project's (ADR-030)
	baseState                  string     // with --env, the project's state store, which holds the base's previous text
	baseTexts                  string     // the project's base texts: the text each base was planned from (ADR-030)
	model, cacheDir            string
	patchModel                 string // empty: model
	effort, patchEffort        string // low, medium or high
	events                     string
	noCache, yes, noEvents     bool
	requireEndpoint, noColor   bool
	ignore                     []string           // files of dir that are not intents
	aws                        engine.AWSSettings // region and endpoint, from the environment
}

// errUsage marks a usage error (exit code 2).
var errUsage = errors.New("usage error")

// parseReconcileFlags parses the arguments of cmd ("plan", "apply", or
// "check" and "recover", which have no planner flags) and loads the project
// in the directory given, the current one by default. The lock, the state
// store, the event log and the planner cache default to the project's; with
// --env (a file, relative to the current directory) the first three are the
// environment's (project.EnvPaths), and --lock, --state and --events still
// win; the project's lock is then the lock of the base (ADR-030). A directory with a prosa.yaml (ADR-027), a missing AWS region and an
// environment file that is not valid or sits in the project root are usage
// errors.
func parseReconcileFlags(cmd string, args []string, stderr io.Writer) (reconcileFlags, error) {
	var f reconcileFlags
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	var ignore string
	fs.StringVar(&ignore, "ignore", "README.md", "comma-separated *.md files of the project root that are not intents")
	fs.StringVar(&f.lock, "lock", "", "lockfile (default <dir>/prosa.lock.yaml)")
	fs.StringVar(&f.state, "state", "", "state store directory (default <dir>/.prosa/state)")
	fs.StringVar(&f.events, "events", "", "event log (default <dir>/.prosa/events.jsonl)")
	fs.StringVar(&f.envFile, "env", "", "environment file (envs/<name>.md): prose that wins over the intents; its lock sits next to it")
	fs.BoolVar(&f.noEvents, "no-events", false, "do not write the event log")
	fs.BoolVar(&f.requireEndpoint, "require-endpoint", false, "refuse to run without an emulated endpoint, of PROSA_AWS_ENDPOINT (also PROSA_REQUIRE_ENDPOINT=1)")
	fs.BoolVar(&f.noColor, "no-color", false, "no ANSI colors (also NO_COLOR; never when stdout is not a terminal)")
	if cmd == "plan" || cmd == "apply" {
		fs.StringVar(&f.fixtures, "fixtures", "", "JSON file of planner fixtures, keyed by the effective hash of the intent (with --env, the intent and environment hashes combined), else by the intent hash")
		fs.StringVar(&f.model, "model", envOr("PROSA_MODEL", planner.DefaultModel), "model of the planner (also PROSA_MODEL)")
		fs.StringVar(&f.patchModel, "patch-model", os.Getenv("PROSA_PATCH_MODEL"), "model of the planner for patches, --model when empty (also PROSA_PATCH_MODEL)")
		fs.BoolVar(&f.noCache, "no-cache", false, "do not read or write the planner cache, nor resume an interrupted intent from its saved desired state")
	}
	if cmd == "apply" {
		fs.BoolVar(&f.yes, "yes", false, "apply without asking")
	}
	if err := fs.Parse(args); err != nil {
		return f, errUsage
	}
	if fs.NArg() > 1 {
		usage(stderr)
		return f, errUsage
	}
	modelSet := false
	fs.Visit(func(fl *flag.Flag) { modelSet = modelSet || fl.Name == "model" || fl.Name == "patch-model" })
	if f.fixtures != "" && modelSet {
		fmt.Fprintln(stderr, "prosa: --fixtures and --model or --patch-model are mutually exclusive")
		usage(stderr)
		return f, errUsage
	}
	if cmd == "plan" || cmd == "apply" {
		err := f.plannerEnv(os.Getenv)
		if err == nil && f.fixtures == "" && os.Getenv("PROSA_LLM") == "openai" && !flagSet(fs, "model") && os.Getenv("PROSA_MODEL") == "" {
			err = errors.New("PROSA_LLM=openai needs --model (or PROSA_MODEL): the default model is an Anthropic one")
		}
		if err != nil {
			fmt.Fprintf(stderr, "prosa: %v\n", err)
			return f, errUsage
		}
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	p, err := project.Open(dir)
	if errors.Is(err, project.ErrLegacyConfig) {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return f, errUsage
	}
	if err != nil {
		return f, err
	}
	aws, err := project.Settings(os.Getenv)
	if errors.Is(err, project.ErrNoRegion) {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return f, errUsage
	}
	if err != nil {
		return f, err
	}
	f.dir, f.aws = p.Root, aws
	for _, name := range strings.Split(ignore, ",") {
		if name = strings.TrimSpace(name); name != "" {
			f.ignore = append(f.ignore, name)
		}
	}
	lockPath, stateDir, eventsPath := p.LockPath(), p.StateDir(), p.EventsPath()
	if f.envFile != "" {
		if f.env, err = intent.LoadEnv(f.envFile); err == nil {
			err = p.CheckEnvLocation(f.env)
		}
		if err != nil {
			fmt.Fprintf(stderr, "prosa: %v\n", err)
			return f, errUsage
		}
		f.baseLock, f.baseState = lockPath, stateDir
		lockPath, stateDir, eventsPath = p.EnvPaths(f.env)
	}
	if f.lock == "" {
		f.lock = lockPath
	}
	if f.state == "" {
		f.state = stateDir
	}
	if f.events == "" {
		f.events = eventsPath
	}
	f.cacheDir, f.baseTexts = p.PlannerCacheDir(), p.BaseTextDir()
	if f.envFile != "" {
		warnEnvSections(stderr, f.env, p.Root, f.ignore, f.lock)
	}
	return f, nil
}

// flagSet reports whether the flag name was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(fl *flag.Flag) { set = set || fl.Name == name })
	return set
}

// warnEnvSections warns, one line on stderr per heading, about an
// environment's "## X" sections that name no intent (ADR-030): their lines
// are used by no intent, and the run goes on. An intent is one of the
// project, or one the environment's lock (lockPath) still has, which the run
// removes: deleting an intent, or all of them to tear the environment down,
// needs no edit of the environment file. With no intent at all the
// environment applies to nothing and is not checked. A project whose intents
// or lock do not load is left to the engine, which reports it.
func warnEnvSections(stderr io.Writer, env intent.Env, dir string, ignore []string, lockPath string) {
	intents, err := intent.Load(dir, ignore...)
	if err != nil {
		return
	}
	lk, _, err := lock.Read(lockPath)
	if err != nil {
		return
	}
	var names []string
	for _, it := range intents {
		names = append(names, it.Name)
	}
	for name := range lk.Intents {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sorted := strings.Join(slices.Sorted(slices.Values(names)), ", ")
	for _, h := range env.UnknownSections(names) {
		fmt.Fprintf(stderr, "prosa: warning: %s: section %q names no intent (intents: %s); its lines are ignored\n", env.Path, "## "+h, sorted)
	}
}

// envOr returns the variable key, or def when it is unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// plannerEnv reads the efforts of the planner's jobs (PROSA_EFFORT,
// PROSA_PATCH_EFFORT: low, medium or high, by default high and low) and
// checks PROSA_SCHEMA_BYTES, which the schema catalog reads (ADR-030,
// ADR-031).
func (f *reconcileFlags) plannerEnv(getenv func(string) string) error {
	for _, e := range []struct {
		key, def string
		dst      *string
	}{{"PROSA_EFFORT", planner.DefaultEffort, &f.effort}, {"PROSA_PATCH_EFFORT", planner.DefaultPatchEffort, &f.patchEffort}} {
		v := getenv(e.key)
		switch v {
		case "":
			v = e.def
		case "low", "medium", "high":
		default:
			return fmt.Errorf("%s=%q: want low, medium or high", e.key, v)
		}
		*e.dst = v
	}
	_, err := engine.SchemaBudget(getenv)
	return err
}

// plannerSettings are the model and effort of each job of the planner
// (ADR-030): model and effort plan, patchModel (model when empty) and
// patchEffort write patches.
type plannerSettings struct {
	model, patchModel, effort, patchEffort string
}

// plannerSettings returns the planner settings of f.
func (f reconcileFlags) plannerSettings() plannerSettings {
	return plannerSettings{model: f.model, patchModel: f.patchModel, effort: f.effort, patchEffort: f.patchEffort}
}

// newEngine returns the engine of the project of f, without a planner nor
// providers.
func (f reconcileFlags) newEngine(store *state.Store) *engine.Engine {
	e := &engine.Engine{
		Dir: f.dir, LockPath: f.lock, Ignore: f.ignore, Region: f.aws.Region, State: store, BaseTextDir: f.baseTexts,
		CheckOptions: checks.Options{Emulated: f.aws.Endpoint != ""},
	}
	if f.envFile != "" {
		env := f.env
		e.Env, e.EnvFile, e.BaseLockPath = &env, f.envFile, f.baseLock
	}
	return e
}

// plannerSource builds the LLM planner of one command; progress lines go to
// log. The planner must not need credentials until its first Plan.
type plannerSource func(s plannerSettings, log io.Writer) engine.Planner

// llmPlanner is the real plannerSource: the planner on the backend the
// environment chooses (planner.NewBackend: PROSA_LLM and credentials),
// created on the first Plan so that a project with nothing to plan needs
// neither a backend nor credentials.
func llmPlanner(s plannerSettings, log io.Writer) engine.Planner {
	return &lazyPlanner{open: func() (engine.Planner, error) {
		b, err := planner.NewBackend(os.Getenv)
		if err != nil {
			return nil, err
		}
		return &planner.LLM{
			Backend: b, Model: s.model, PatchModel: s.patchModel, Effort: s.effort, PatchEffort: s.patchEffort,
			Log: log, LogIntent: true,
		}, nil
	}}
}

// lazyPlanner opens its planner on the first Plan.
type lazyPlanner struct {
	open func() (engine.Planner, error)
	once sync.Once
	p    engine.Planner
	err  error
}

func (l *lazyPlanner) Plan(ctx context.Context, in engine.PlanInput) (engine.Desired, error) {
	l.once.Do(func() { l.p, l.err = l.open() })
	if l.err != nil {
		return engine.Desired{}, l.err
	}
	return l.p.Plan(ctx, in)
}

// LastResult forwards the result of the planner, when it reports one.
func (l *lazyPlanner) LastResult() planner.Result {
	if rr, ok := l.p.(planner.ResultReporter); ok {
		return rr.LastResult()
	}
	return planner.Result{}
}

// providerFunc is Engine.Providers.
type providerFunc = func(ctx context.Context, name, version string) (engine.Provider, error)

// providerSource opens the providers of one command, configuring
// hashicorp/aws with aws; close stops them.
type providerSource func(aws engine.AWSSettings) (get providerFunc, close func(), err error)

// poolProviders installs providers from the public registry and runs them in
// a ProviderPool.
func poolProviders(aws engine.AWSSettings) (providerFunc, func(), error) {
	reg, err := registry.NewClient()
	if err != nil {
		return nil, nil, err
	}
	pool := engine.NewProviderPool(reg, aws)
	return pool.Get, pool.Close, nil
}

// errChecksFailed is the error of a command whose checks failed.
var errChecksFailed = errors.New("verification failed")

// printChecks writes one line per check result and returns errChecksFailed
// when one failed.
func printChecks(w io.Writer, results []engine.CheckResult) error {
	for _, r := range results {
		line := fmt.Sprintf("check %s: %s — %s", r.Intent, r.Status, r.Name)
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		fmt.Fprintln(w, line)
	}
	if engine.HasFailedChecks(results) {
		return errChecksFailed
	}
	return nil
}

// checkCommand runs prosa check: every check of the lock on the resources
// read again from the providers, one line per check, after a warning per
// intent whose file changed since the last apply; exit code 1 when one
// failed.
func checkCommand(ctx context.Context, args []string, stdout, stderr io.Writer, src providerSource) int {
	f, err := parseReconcileFlags("check", args, stderr)
	if err == nil {
		err = announceTarget(f, stderr)
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	if err == nil {
		err = withEvents(f, "check", stderr, func(log *events.Log) error { return runCheck(ctx, f, stdout, src, log) })
	}
	if err != nil {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return 1
	}
	return 0
}

func runCheck(ctx context.Context, f reconcileFlags, stdout io.Writer, src providerSource, log *events.Log) error {
	store, err := openStore(f)
	if err != nil {
		return err
	}
	get, closeProviders, err := src(f.aws)
	if err != nil {
		return err
	}
	defer closeProviders()
	e := f.newEngine(store)
	e.Providers, e.Observer = get, observer(log)
	results, changed, err := e.Check(ctx)
	if err != nil {
		return err
	}
	for _, name := range changed {
		fmt.Fprintf(stdout, "warning: %s changed since the last apply; checks are the applied ones\n", name)
	}
	return printChecks(stdout, results)
}

// recoverCommand runs prosa recover: it rebuilds from the state store the
// lock entry of each intent whose state is ahead of the lock (ADR-024), one
// line per intent, or says there is nothing to recover.
func recoverCommand(args []string, stdout, stderr io.Writer) int {
	f, err := parseReconcileFlags("recover", args, stderr)
	if err == nil {
		err = announceTarget(f, stderr)
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	if err == nil {
		err = withEvents(f, "recover", stderr, func(*events.Log) error { return runRecover(f, stdout) })
	}
	if err != nil {
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return 1
	}
	return 0
}

func runRecover(f reconcileFlags, stdout io.Writer) error {
	store, err := openStore(f)
	if err != nil {
		return err
	}
	recovered, err := f.newEngine(store).Recover()
	if err != nil {
		return err
	}
	if len(recovered) == 0 {
		fmt.Fprintln(stdout, "nothing to recover")
	}
	for _, r := range recovered {
		fmt.Fprintf(stdout, "recovered %s: %d resource(s) from state serial %d; run plan again\n", r.Intent, r.Resources, r.Serial)
	}
	return nil
}

// openStore opens the state store of f with PROSA_STATE_PASSPHRASE.
func openStore(f reconcileFlags) (*state.Store, error) {
	passphrase := os.Getenv("PROSA_STATE_PASSPHRASE")
	if passphrase == "" {
		return nil, errors.New("PROSA_STATE_PASSPHRASE is not set: it encrypts the state store")
	}
	return state.Open(f.state, passphrase)
}

// reconcile runs plan or apply and returns the exit code.
func reconcile(ctx context.Context, cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer, src providerSource, psrc plannerSource) int {
	f, err := parseReconcileFlags(cmd, args, stderr)
	if err == nil {
		err = announceTarget(f, stderr)
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	if err == nil {
		err = withEvents(f, cmd, stderr, func(log *events.Log) error {
			return runReconcile(ctx, cmd, f, stdin, stdout, stderr, src, psrc, log)
		})
	}
	switch {
	case errors.Is(err, engine.ErrNotApproved):
		fmt.Fprintf(stderr, "prosa: %v: nothing was changed\n", err)
		return 1
	case errors.As(err, new(reportedError)):
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "prosa: %v\n", err)
		return 1
	}
	return 0
}

func runReconcile(ctx context.Context, cmd string, f reconcileFlags, stdin io.Reader, stdout, stderr io.Writer, src providerSource, psrc plannerSource, log *events.Log) error {
	store, err := openStore(f)
	if err != nil {
		return err
	}
	e := f.newEngine(store)
	if f.baseState != "" {
		if e.BaseState, err = state.Open(f.baseState, os.Getenv("PROSA_STATE_PASSPHRASE")); err != nil {
			return err
		}
	}
	e.Observer, e.InFlight, e.NoResume = observer(log), inFlight, f.noCache
	if f.fixtures != "" {
		sp, err := engine.LoadFixtures(f.fixtures)
		if err != nil {
			return err
		}
		e.Planner = fixturesPlanner(sp, log)
	} else {
		var pl engine.Planner = psrc(f.plannerSettings(), stderr)
		if !f.noCache {
			pl = &planner.Cached{Inner: pl, Dir: f.cacheDir, Model: f.model, PatchModel: f.patchModel}
		}
		e.Planner = withPlannerEvents(pl, log)
	}
	get, closeProviders, err := src(f.aws)
	if err != nil {
		return err
	}
	defer closeProviders()
	e.Providers = get

	p, err := e.Plan(ctx)
	if err != nil {
		return err
	}
	color := useColor(f.noColor, stdout)
	engine.Render(stdout, p, engine.RenderOptions{Color: color})
	if p.Blocked != "" {
		return fmt.Errorf("plan blocked: %s", p.Blocked)
	}
	if p.IsBlocked() {
		return fmt.Errorf("plan blocked: %s", strings.Join(p.Blockers, "; "))
	}
	if !p.HasChanges() && p.LockDirty {
		fmt.Fprintln(stdout, "The lock will be updated (no infrastructure changes).")
	}
	if cmd == "plan" {
		return nil
	}
	// Only the changes that need an approval are asked about; refused ones
	// (observe, advise) are never applied, even with --yes.
	approve := func(p *engine.Plan) bool {
		if f.yes {
			return true
		}
		engine.RenderApproval(stdout, p, engine.RenderOptions{Color: color})
		fmt.Fprint(stdout, approvePrompt)
		return readYes(ctx, stdin, stdout)
	}
	progress := newProgressPrinter(stdout, stderr, progressClock, color)
	e.Progress = progress.handle
	err = e.Apply(ctx, p, approve)
	progress.stopTicker() // a no-op unless an operation never reported its end
	if err != nil {
		if progress.reported(err) {
			return reportedError{err}
		}
		return err
	}
	printApplySummary(stdout, p, progress, color)
	return printChecks(stdout, p.CheckResults)
}

// reportedError is an error the progress lines already printed: the
// command exits with 1 without printing it again.
type reportedError struct{ err error }

func (e reportedError) Error() string { return e.err.Error() }
func (e reportedError) Unwrap() error { return e.err }

// readYes reads one line of stdin and reports whether it is "yes"; when
// stdin is not a terminal (the answer was piped, not typed with its Enter)
// it ends the prompt's line. A cancelled ctx (Ctrl-C at the prompt) is a no
// at once: the read goes on in its goroutine until the process exits.
func readYes(ctx context.Context, stdin io.Reader, stdout io.Writer) bool {
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		answer <- line
	}()
	select {
	case line := <-answer:
		if !isTerminal(stdin) {
			fmt.Fprintln(stdout)
		}
		return strings.TrimSpace(line) == "yes"
	case <-ctx.Done():
		fmt.Fprintln(stdout)
		return false
	}
}

// printApplySummary writes, after an Apply that wrote or applied something,
// what was not applied, if anything (refused changes: never applicable, not
// approved, or depending on one of those), then "Apply complete! Resources:
// N added, M changed, K destroyed." with the operations that completed.
func printApplySummary(w io.Writer, p *engine.Plan, progress *progressPrinter, color bool) {
	if !p.Written && !p.Applicable() {
		return
	}
	engine.RenderNotApplied(w, p)
	writeApplyComplete(w, progress, color)
}
