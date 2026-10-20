package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/giammbo/prosa/internal/engine"
)

// clock is the time of the progress lines; a fake one in tests.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// progressClock is the clock of apply's progress lines.
var progressClock clock = realClock{}

// stillInterval is how often a running operation says it still runs.
const stillInterval = 10 * time.Second

// progressPrinter writes Terraform-style lines for the operations of an
// apply (engine.Engine.Progress): "<intent>/<key>: Creating...", every 10s
// "Still creating... [10s elapsed]", then "Creation complete after 1m12s
// [id=...]"; a failure is "Error: <error>" on errOut. It counts the
// completed operations for the "Apply complete!" line.
type progressPrinter struct {
	out, errOut io.Writer
	clock       clock
	color       bool

	mu     sync.Mutex // serializes the writes of the ticker and of handle
	start  time.Time  // of the operation in progress
	stop   chan struct{}
	ticker chan struct{} // closed when the ticker of the operation exits
	failed []error       // the errors printed

	added, changed, destroyed int
}

func newProgressPrinter(out, errOut io.Writer, c clock, color bool) *progressPrinter {
	return &progressPrinter{out: out, errOut: errOut, clock: c, color: color}
}

// verbs of each action: the start, the ticker, the completion.
var progressVerbs = map[engine.Action][3]string{
	engine.Create: {"Creating...", "Still creating...", "Creation complete"},
	engine.Update: {"Modifying...", "Still modifying...", "Modifications complete"},
	engine.Delete: {"Destroying...", "Still destroying...", "Destruction complete"},
}

// handle prints one phase of an operation; it is called by Apply, one
// operation at a time.
func (p *progressPrinter) handle(ev engine.OpProgress) {
	verbs := progressVerbs[ev.Action]
	addr := ev.Intent + "/" + ev.Key
	switch ev.Phase {
	case engine.OpStart:
		p.stopTicker()
		p.start = p.clock.Now()
		line := verbs[0]
		if ev.ID != "" {
			line += " [id=" + ev.ID + "]"
		}
		p.println(addr, line)
		p.startTicker(addr, verbs[1], ev.ID)
	case engine.OpDone:
		p.stopTicker()
		line := verbs[2] + " after " + elapsed(p.clock.Now().Sub(p.start))
		if ev.ID != "" {
			line += " [id=" + ev.ID + "]"
		}
		p.println(addr, line)
		switch ev.Action {
		case engine.Create:
			p.added++
		case engine.Update:
			p.changed++
		case engine.Delete:
			p.destroyed++
		}
	case engine.OpFailed:
		p.stopTicker()
		p.mu.Lock()
		fmt.Fprintf(p.errOut, "Error: %v\n", ev.Err)
		p.failed = append(p.failed, ev.Err)
		p.mu.Unlock()
	}
}

// reported reports whether err is, or wraps, an error handle printed.
func (p *progressPrinter) reported(err error) bool {
	for _, f := range p.failed {
		if err != nil && errors.Is(err, f) {
			return true
		}
	}
	return false
}

func (p *progressPrinter) println(addr, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, "%s %s\n", paint(p.color, ansiBold, addr+":"), text)
}

// startTicker prints "<still> [<elapsed> elapsed]" every stillInterval
// until stopTicker.
func (p *progressPrinter) startTicker(addr, still, id string) {
	stop, done := make(chan struct{}), make(chan struct{})
	p.stop, p.ticker = stop, done
	start := p.start
	go func() {
		defer close(done)
		for {
			select {
			case <-p.clock.After(stillInterval):
			case <-stop:
				return
			}
			select {
			case <-stop: // stopped while the tick fired
				return
			default:
			}
			info := elapsed(p.clock.Now().Sub(start)) + " elapsed"
			if id != "" {
				info = "id=" + id + ", " + info
			}
			p.println(addr, still+" ["+info+"]")
		}
	}()
}

// stopTicker stops the ticker of the operation in progress, if any, and
// waits for it: no line of it follows.
func (p *progressPrinter) stopTicker() {
	if p.stop == nil {
		return
	}
	close(p.stop)
	<-p.ticker
	p.stop, p.ticker = nil, nil
}

// elapsed is a duration as Terraform writes it: whole seconds, "1m12s".
func elapsed(d time.Duration) string { return d.Truncate(time.Second).String() }

// writeApplyComplete writes "Apply complete! Resources: N added, M changed,
// K destroyed." with the counts of p.
func writeApplyComplete(w io.Writer, p *progressPrinter, color bool) {
	line := fmt.Sprintf("Apply complete! Resources: %d added, %d changed, %d destroyed.", p.added, p.changed, p.destroyed)
	fmt.Fprintln(w, paint(color, ansiBold+ansiGreen, line))
}
