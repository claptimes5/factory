package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// ExecFunc returns the command a live terminal runs for a job.
type ExecFunc func(ctx context.Context, jobID int64) (*exec.Cmd, error)

// DockerExec opens a shell in a running job's container.
func DockerExec(st *store.Store) ExecFunc {
	return func(ctx context.Context, jobID int64) (*exec.Cmd, error) {
		j, err := st.GetJob(ctx, jobID)
		if err != nil {
			return nil, err
		}
		if j.State != state.Running {
			return nil, fmt.Errorf("job %d is %s, not running", jobID, j.State)
		}
		return exec.CommandContext(ctx, "docker", "exec", "-it", fmt.Sprintf("factory-job-%d", jobID), "sh"), nil
	}
}

// termMsg is what the browser sends: keystrokes or a new window size.
type termMsg struct {
	Type string `json:"type"` // input|resize
	Data string `json:"data"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// apiTerminal attaches a browser terminal to a running job over WebSocket.
// The terminal's output goes out as binary frames; termMsg comes in as text.
// The handshake only accepts this server's own origin (the websocket
// package's default), so another site open in the browser cannot attach.
func (s *Server) apiTerminal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if s.Exec == nil {
		apiErr(w, http.StatusNotImplemented, "this runner has no containers to attach to")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd, err := s.Exec(ctx, id)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(1 << 20)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, trim(err.Error(), 100))
		return
	}
	defer func() {
		_ = f.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				werr := c.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				_ = c.Close(websocket.StatusNormalClosure, "session ended")
				return
			}
		}
	}()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m termMsg
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		switch m.Type {
		case "input":
			if _, err := f.Write([]byte(m.Data)); err != nil {
				return
			}
		case "resize":
			if m.Cols > 0 && m.Rows > 0 {
				_ = pty.Setsize(f, &pty.Winsize{Cols: m.Cols, Rows: m.Rows})
			}
		}
	}
}
