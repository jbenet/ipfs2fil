package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// progress draws one status line for a long step: a bar with ETA when the
// total is known, a running count when it is not. On a terminal it redraws in
// place; otherwise (logs, CI) it prints a line every interval. The bar also
// shows the speed over the last speedWindow, which drives the ETA.
type progress struct {
	w        io.Writer
	label    string
	total    int64 // 0 if unknown
	bytes    bool  // format counts as bytes
	tty      bool
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	suffix  string
	done    int64
	skipped int64 // part of done that did not move through here
	start   time.Time
	last    time.Time
	drawn   bool
	samples []sample // moved units over time, for the speed
}

type sample struct {
	t time.Time
	n int64
}

const speedWindow = 20 * time.Second

func newProgress(w io.Writer, label string, total int64, bytes bool) *progress {
	p := &progress{w: w, label: label, total: total, bytes: bytes, tty: isTerminal(w), now: time.Now}
	p.interval = 100 * time.Millisecond
	if !p.tty {
		p.interval = 10 * time.Second
	}
	p.start = p.now()
	p.last = p.start
	return p
}

// Add records n more units done.
func (p *progress) Add(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done += n
	if t := p.now(); !p.drawn || t.Sub(p.last) >= p.interval {
		p.last = t
		p.draw(false)
	}
}

// Skip records n more units done that did not move through here, such as
// pieces a provider already held. They fill the bar but not the speed.
func (p *progress) Skip(n int64) {
	p.mu.Lock()
	p.skipped += n
	p.mu.Unlock()
	p.Add(n)
}

// Set records done units out of total, for callers that report running totals.
func (p *progress) Set(done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done, p.total = done, total
	if t := p.now(); t.Sub(p.last) >= p.interval {
		p.last = t
		p.draw(false)
	}
}

// Suffix sets a note shown after the bar, or clears it when empty. On a
// terminal a changed note is drawn at once, as it may be the only news for a
// while.
func (p *progress) Suffix(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s == p.suffix {
		return
	}
	p.suffix = s
	if p.tty {
		p.last = p.now()
		p.draw(false)
	}
}

// Log prints a line above the bar, then redraws the bar.
func (p *progress) Log(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty {
		fmt.Fprintf(p.w, "\r\033[K%s\n", line)
		p.draw(false)
		return
	}
	fmt.Fprintln(p.w, line)
}

// Done draws the final line.
func (p *progress) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty || p.drawn {
		p.draw(true)
	}
}

// speed is units per second over the last speedWindow, or 0 before a second
// has passed.
func (p *progress) speed(t time.Time) float64 {
	moved := p.done - p.skipped
	if p.samples == nil {
		p.samples = []sample{{t: p.start}}
	}
	if s := p.samples[len(p.samples)-1]; t.Sub(s.t) >= time.Second {
		p.samples = append(p.samples, sample{t, moved})
	}
	i := 0
	for i < len(p.samples)-2 && t.Sub(p.samples[i+1].t) >= speedWindow {
		i++
	}
	p.samples = p.samples[i:]
	s := p.samples[0]
	dt := t.Sub(s.t).Seconds()
	if dt < 1 {
		return 0
	}
	return float64(moved-s.n) / dt
}

func (p *progress) rate(perSec float64) string {
	return p.count(int64(perSec)) + "/s"
}

func (p *progress) draw(final bool) {
	p.drawn = true
	t := p.now()
	el := t.Sub(p.start)
	speed := p.speed(t)
	var line string
	if p.total > 0 {
		frac := float64(p.done) / float64(p.total)
		if frac > 1 {
			frac = 1
		}
		const width = 24
		fill := int(frac * width)
		bar := strings.Repeat("=", fill) + strings.Repeat(" ", width-fill)
		line = fmt.Sprintf("%s [%s] %3.0f%% %s/%s", p.label, bar, frac*100, p.count(p.done), p.count(p.total))
		if final {
			line += " in " + roundDur(el)
			if s := el.Seconds(); s >= 1 && p.done > p.skipped {
				line += " (" + p.rate(float64(p.done-p.skipped)/s) + ")"
			}
		} else if speed > 0 {
			line += " " + p.rate(speed)
			if left := p.total - p.done; left > 0 {
				line += " ETA " + roundDur(time.Duration(float64(left)/speed*float64(time.Second)))
			}
		}
	} else {
		line = fmt.Sprintf("%s %s", p.label, p.count(p.done))
		if s := el.Seconds(); s >= 1 {
			line += fmt.Sprintf(" (%s/s)", p.count(int64(float64(p.done)/s)))
		}
		if final {
			line += " in " + roundDur(el)
		}
	}
	if p.suffix != "" && !final {
		line += " · " + p.suffix
	}
	if p.tty {
		fmt.Fprintf(p.w, "\r\033[K%s", line)
		if final {
			fmt.Fprintln(p.w)
		}
	} else {
		fmt.Fprintln(p.w, line)
	}
}

func (p *progress) count(n int64) string {
	if p.bytes {
		return humanBytes(n)
	}
	return fmt.Sprint(n)
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// confirm asks a yes/no question on stderr and reads the answer from in. It
// says no when in is not a terminal, so scripts must pass --yes.
func confirm(o *opts, in io.Reader, question string) (bool, error) {
	if o.yes {
		return true, nil
	}
	if f, ok := in.(*os.File); ok && !isTerminal(f) {
		return false, fmt.Errorf("%s needs a yes; pass --yes to run without asking", question)
	}
	fmt.Fprintf(o.stderr, "%s [y/N] ", question)
	ans, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && ans == "" {
		if err == io.EOF { // e.g. stdin is /dev/null, which still looks like a terminal
			fmt.Fprintln(o.stderr)
			return false, fmt.Errorf("%s needs a yes; pass --yes to run without asking", question)
		}
		return false, err
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes", nil
}

// stdin is where confirm reads answers. Tests replace it.
var stdin io.Reader = os.Stdin

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
