package main

import (
	"fmt"
	"os"
	"sync"
	"time"
	"tummy/internal/txt"

	"github.com/mattn/go-isatty"

	"tummy/internal/engine"
)

// progress draws a one-line live status on stderr while a run is going, on a terminal
// only. Finished streams scroll up as fixed lines; the current one updates in place.
type progress struct {
	mu      sync.Mutex
	cur     engine.Event
	started time.Time
	active  bool
	stop    chan struct{}
	done    chan struct{}
	tty     bool
}

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newProgress(enabled bool) *progress {
	p := &progress{tty: enabled && (isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()))}
	if p.tty {
		p.stop, p.done = make(chan struct{}), make(chan struct{})
		go p.loop()
	}
	return p
}

func (p *progress) handle(ev engine.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch ev.Phase {
	case engine.PhaseDone, engine.PhaseFailed:
		took := time.Since(p.started).Round(time.Millisecond)
		p.active = false
		if !p.tty {
			return // the log has a line per stream
		}
		p.clear()
		if ev.Err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s → %s  %s  %s\n", ev.Stream, ev.Object, oneLine(ev.Err.Error()), took)
		} else {
			fmt.Fprintf(os.Stderr, "✓ %s → %s  %s rows  %s\n", ev.Stream, ev.Object, txt.Thousands(ev.Rows), took)
		}
		return
	case engine.PhaseQuerying:
		p.started = time.Now()
	}
	p.cur = ev
	p.active = true
	if !p.tty {
		return
	}
	p.draw()
}

func (p *progress) line() string {
	ev := p.cur
	frame := frames[int(time.Now().UnixMilli()/80)%len(frames)]
	switch ev.Phase {
	case engine.PhaseConnecting:
		return fmt.Sprintf("%s connecting %s", frame, ev.Object)
	case engine.PhaseQuerying:
		return fmt.Sprintf("%s %s  querying  %s", frame, ev.Stream, txt.Dur(time.Since(p.started)))
	case engine.PhaseParents:
		return fmt.Sprintf("%s %s  parent %s %s  %s", frame, ev.Stream, ev.Object, txt.Thousands(ev.Rows), txt.Dur(time.Since(p.started)))
	case engine.PhaseFetching:
		if n := ev.Note(); n != "" {
			return fmt.Sprintf("%s %s  fetching %s  %s  %s", frame, ev.Stream, txt.Thousands(ev.Rows), n, txt.Dur(time.Since(p.started)))
		}
		return fmt.Sprintf("%s %s  fetching %s  %s", frame, ev.Stream, txt.Thousands(ev.Rows), txt.Dur(time.Since(p.started)))
	case engine.PhaseLoading:
		return fmt.Sprintf("%s %s → %s  loading %s  %s", frame, ev.Stream, ev.Object, txt.Thousands(ev.Rows), txt.Dur(time.Since(p.started)))
	case engine.PhaseMerging:
		return fmt.Sprintf("%s %s → %s  merging %s  %s", frame, ev.Stream, ev.Object, txt.Thousands(ev.Rows), txt.Dur(time.Since(p.started)))
	}
	return ""
}

func (p *progress) draw() {
	fmt.Fprint(os.Stderr, "\r\033[2K"+p.line())
}

func (p *progress) clear() {
	if p.tty {
		fmt.Fprint(os.Stderr, "\r\033[2K")
	}
}

func (p *progress) loop() {
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			p.mu.Lock()
			p.clear()
			p.mu.Unlock()
			close(p.done)
			return
		case <-t.C:
			p.mu.Lock()
			if p.active {
				p.draw()
			}
			p.mu.Unlock()
		}
	}
}

func (p *progress) finish() {
	if !p.tty {
		return
	}
	close(p.stop)
	<-p.done
}
