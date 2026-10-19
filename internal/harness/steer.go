package harness

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Steering: a person on the job page can send a running builder a
// message, which it reads at its next step. Only a CLI that takes its
// input as a stream of messages can (Claude, with --input-format
// stream-json); its stdin then stays open while it runs, and closes once
// it has answered everything it was sent.

// SteerMsg is a message for a running agent. Err gets nil once it was
// written to the agent's input, or why it wasn't.
type SteerMsg struct {
	Text string
	Err  chan<- error
}

// ErrNotListening is a message for a run that no longer reads its input.
var ErrNotListening = errors.New("the agent has finished and no longer reads messages")

// steerable is an adapter whose CLI reads further messages while it runs.
type steerable interface {
	// userLine is one message on the CLI's input.
	userLine(text string) []byte
	// observe reads one line of output: ack when it is a message from the
	// input coming back (the CLI took it), done when the run ended (a
	// result), and stopped when it ended on a cap, after which no message
	// is answered.
	observe(line []byte) (ack, done, stopped bool)
}

// idleClose is how long the input stays open after a result while a
// message the CLI hasn't taken back is outstanding: it may start on it,
// or it may have folded it into the run that just ended.
var idleClose = time.Minute

// feeder is a run's open input. Its read end is a real file, so the
// runner hands it to the process as is (no copying goroutine that would
// keep the run from ending while the input is open).
type feeder struct {
	mu      sync.Mutex
	r, w    *os.File
	a       steerable
	closed  bool
	pending int // messages sent since the last result and not taken back
	timer   *time.Timer
}

func newFeeder(a steerable) (*feeder, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &feeder{r: r, w: w, a: a}, nil
}

// send writes a message; steer counts it as one the CLI must take back
// before the input closes.
func (f *feeder) send(text string, steer bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrNotListening
	}
	// A pipe's buffer holds a message or two; the prompt goes in from a
	// goroutine (see Run) so a large one can't block the start.
	if _, err := f.w.Write(f.a.userLine(text)); err != nil {
		return err
	}
	if steer {
		f.pending++
	}
	return nil
}

// saw is one line of the CLI's output.
func (f *feeder) saw(line []byte) {
	ack, done, stopped := f.a.observe(line)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timer != nil {
		f.timer.Stop() // it's working on what it was sent
		f.timer = nil
	}
	if ack && f.pending > 0 {
		f.pending--
	}
	if !done {
		return
	}
	if stopped || f.pending == 0 {
		f.closeLocked()
		return
	}
	f.timer = time.AfterFunc(idleClose, f.close)
}

func (f *feeder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeLocked()
}

func (f *feeder) closeLocked() {
	if f.closed {
		return
	}
	f.closed = true
	_ = f.w.Close()
}

// release closes both ends once the run is over.
func (f *feeder) release() {
	f.close()
	f.mu.Lock()
	if f.timer != nil {
		f.timer.Stop()
	}
	f.mu.Unlock()
	_ = f.r.Close()
}

// NoteLine is a line the factory adds to a run's log: a message a
// person sent it ("you"), so the transcript shows it where it went in.
func NoteLine(text string, delivered bool) []byte {
	b, _ := json.Marshal(map[string]any{"type": "factory_message", "from": "you", "text": text, "delivered": delivered})
	return b
}

// ReadJobFile reads a small file a job wrote under outDir, safely (see
// readJobFile).
func ReadJobFile(outDir, name string) ([]byte, error) { return readJobFile(outDir, name) }
