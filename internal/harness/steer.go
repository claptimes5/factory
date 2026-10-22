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
	// observe reads one line of output: echo is the text of a message from
	// the input coming back (the CLI took it), ok whether the line was
	// one; done when a turn ended (a result), and stopped when it ended
	// on a cap, after which no message is answered. tasks is how many
	// background tasks the agent has running, when the line says, else -1:
	// a turn that ends with some still running isn't the run's end, since
	// the agent takes another turn once they finish.
	observe(line []byte) (echo string, ok, done, stopped bool, tasks int)
}

// idleClose is how long the input stays open after a result while a
// message the CLI hasn't taken back is outstanding: it may start on it,
// or it may have folded it into the run that just ended.
var idleClose = time.Minute

// feeder is a run's open input. Its read end is a real file, so the
// runner hands it to the process as is (no copying goroutine that would
// keep the run from ending while the input is open). One goroutine
// writes, in order: the prompt first, then each message. Nothing holds
// the lock across a write, so a CLI that never reads its input (it
// failed to start) can't wedge the run: release closes the pipe, which
// fails the blocked write.
type feeder struct {
	mu      sync.Mutex
	r, w    *os.File
	a       steerable
	closed  bool       // takes no more messages
	queue   []feedLine // to write
	unacked []string   // messages written or queued, not yet taken back
	tasks   int        // the agent's background tasks running
	timer   *time.Timer
	wake    chan struct{}
	gone    chan struct{}
	once    sync.Once
	writer  sync.WaitGroup
}

type feedLine struct {
	line []byte
	done chan error // nil for the prompt
}

// newFeeder opens the input with the prompt as its first message.
func newFeeder(a steerable, prompt string) (*feeder, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	f := &feeder{r: r, w: w, a: a, wake: make(chan struct{}, 1), gone: make(chan struct{})}
	f.queue = []feedLine{{line: a.userLine(prompt)}}
	f.writer.Add(1)
	go f.write()
	return f, nil
}

// send queues a message, which the CLI must take back before the input
// closes. The returned channel says once it was written, or why not.
func (f *feeder) send(text string) <-chan error {
	done := make(chan error, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		done <- ErrNotListening
		return done
	}
	f.queue = append(f.queue, feedLine{line: f.a.userLine(text), done: done})
	f.unacked = append(f.unacked, text)
	f.poke()
	return done
}

func (f *feeder) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// write is the one goroutine that writes the input, and closes it once
// it's closed and everything queued went in.
func (f *feeder) write() {
	defer f.writer.Done()
	for {
		f.mu.Lock()
		if len(f.queue) == 0 {
			closed := f.closed
			f.mu.Unlock()
			if closed {
				_ = f.w.Close()
				return
			}
			select {
			case <-f.wake:
			case <-f.gone:
			}
			continue
		}
		l := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		_, err := f.w.Write(l.line)
		if err != nil {
			err = ErrNotListening
		}
		if l.done != nil {
			l.done <- err
		}
		if err != nil {
			f.mu.Lock()
			f.closed = true
			for _, q := range f.queue {
				if q.done != nil {
					q.done <- ErrNotListening
				}
			}
			f.queue = nil
			f.mu.Unlock()
			_ = f.w.Close()
			return
		}
	}
}

// saw is one line of the CLI's output.
func (f *feeder) saw(line []byte) {
	echo, ok, done, stopped, tasks := f.a.observe(line)
	f.mu.Lock()
	defer f.mu.Unlock()
	if tasks >= 0 {
		f.tasks = tasks
	}
	if f.timer != nil {
		f.timer.Stop() // it's working on what it was sent
		f.timer = nil
	}
	if ok {
		// Only a message the factory sent counts: the prompt's own echo
		// doesn't.
		for i, t := range f.unacked {
			if t == echo {
				f.unacked = append(f.unacked[:i], f.unacked[i+1:]...)
				break
			}
		}
	}
	if !done {
		return
	}
	switch {
	case stopped:
		f.closeLocked()
	case f.tasks > 0:
		// Another turn comes when they finish, and takes messages.
	case len(f.unacked) == 0:
		f.closeLocked()
	default:
		f.timer = time.AfterFunc(idleClose, f.close)
	}
}

func (f *feeder) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeLocked()
}

// closeLocked takes no more messages; the writer closes the input once
// what's queued went in.
func (f *feeder) closeLocked() {
	f.closed = true
	f.poke()
}

// release ends the input once the run is over: a write still blocked
// (nothing reads it any more) fails, and the writer is gone on return.
func (f *feeder) release() {
	f.once.Do(func() {
		f.mu.Lock()
		f.closed = true
		if f.timer != nil {
			f.timer.Stop()
		}
		f.mu.Unlock()
		close(f.gone)
		_ = f.r.Close()
		_ = f.w.Close()
		f.writer.Wait()
	})
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
