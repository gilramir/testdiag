// Package stdin is the single reader of os.Stdin. It muxes incoming lines
// between run_script confirmation prompts and operator-interrupt messages for
// the tool-using inspection stages (PLANINSPECTION, DEEPINSPECT).
package stdin

import (
	"bufio"
	"context"
	"os"
	"sync"
)

// InterruptController lets an operator inject guidance into a running
// DEEPINSPECT or PLANINSPECTION run. It is also the sole reader of os.Stdin,
// routing lines to run_script's confirmation prompt when one is active and to
// the interrupt channel otherwise — ensuring only one goroutine ever calls
// read(2) on stdin.
//
// Usage:
//  1. Call New then WatchStdin once at program startup.
//  2. Pass ic.ConfirmLine to tools.SetStdinReader so run_script uses this
//     controller instead of reading os.Stdin directly.
//  3. Pass ic to the inspect.Engine via Options.Interrupt for DEEPINSPECT.
//  4. Pass ic.Drain to the Diagnoser so stale messages are discarded between
//     hypothesis runs.
type InterruptController struct {
	interruptCh chan string   // operator interrupt messages; buffer 4
	confirmCh   chan string   // run_script confirmation responses; buffer 1
	done        chan struct{} // closed when WatchStdin goroutine exits

	mu      sync.Mutex
	confirm bool     // true while run_script is waiting for a response
	sticky  []string // operator notes accumulated during the current inspection run
}

// New allocates the controller. Call WatchStdin to start the stdin reader.
func New() *InterruptController {
	return &InterruptController{
		interruptCh: make(chan string, 4),
		confirmCh:   make(chan string, 1),
		done:        make(chan struct{}),
	}
}

// WatchStdin starts the single goroutine that reads all stdin lines and routes
// them: to confirmCh when a run_script prompt is active, to interruptCh
// otherwise. Call at most once; the goroutine exits on EOF.
func (ic *InterruptController) WatchStdin() {
	scanner := bufio.NewScanner(os.Stdin)
	go func() {
		defer close(ic.done)
		for scanner.Scan() {
			line := scanner.Text()
			ic.mu.Lock()
			inConfirm := ic.confirm
			ic.mu.Unlock()
			if inConfirm {
				select {
				case ic.confirmCh <- line:
				default:
				}
			} else {
				select {
				case ic.interruptCh <- line:
				default:
				}
			}
		}
	}()
}

// ConfirmLine is passed to tools.SetStdinReader. It switches to confirm mode,
// blocks until a line arrives (or stdin closes), then restores normal mode.
// Returns "" if stdin closes before a line is available (safe-fail: decline).
func (ic *InterruptController) ConfirmLine() string {
	ic.mu.Lock()
	ic.confirm = true
	ic.mu.Unlock()
	defer func() {
		ic.mu.Lock()
		ic.confirm = false
		ic.mu.Unlock()
	}()
	select {
	case line := <-ic.confirmCh:
		return line
	case <-ic.done:
		return ""
	}
}

// TakeNonBlocking returns a pending interrupt message and true if one is
// queued, or ("", false) without blocking.
func (ic *InterruptController) TakeNonBlocking() (string, bool) {
	select {
	case line := <-ic.interruptCh:
		return line, true
	default:
		return "", false
	}
}

// ReadLine blocks until an interrupt message is available, ctx is cancelled,
// or stdin closes.
func (ic *InterruptController) ReadLine(ctx context.Context) (string, bool) {
	select {
	case line := <-ic.interruptCh:
		return line, true
	case <-ctx.Done():
		return "", false
	case <-ic.done:
		return "", false
	}
}

// Drain discards all queued interrupt messages and clears accumulated operator
// notes. Call between hypothesis runs to start each run with a clean slate.
func (ic *InterruptController) Drain() {
	for {
		select {
		case <-ic.interruptCh:
		default:
			ic.mu.Lock()
			ic.sticky = nil
			ic.mu.Unlock()
			return
		}
	}
}

// AddStickyNote records an operator message to re-inject into every subsequent
// LLM request in this inspection run.
func (ic *InterruptController) AddStickyNote(note string) {
	ic.mu.Lock()
	ic.sticky = append(ic.sticky, note)
	ic.mu.Unlock()
}

// StickyNotes returns a snapshot of accumulated operator notes.
func (ic *InterruptController) StickyNotes() []string {
	ic.mu.Lock()
	out := append([]string(nil), ic.sticky...)
	ic.mu.Unlock()
	return out
}
