// Package web is the control room: server-rendered HTML with htmx, embedded
// in the binary. It binds to localhost by default; reach it remotely over
// Tailscale or an SSH tunnel. There is no login in v1, so never expose it on
// a public port.
package web

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Actions are the buttons: every one goes through the daemon, which checks
// it like any other request.
type Actions interface {
	Retry(ctx context.Context, jobID int64, tierUp bool) error
	Cancel(ctx context.Context, jobID int64) error
	Park(ctx context.Context, itemID int64, reason string) error
}

// Server serves the control room.
type Server struct {
	Store      *store.Store
	Actions    Actions
	Repo       string
	DailyCap   float64
	AwaitCap   int
	Attach     func(ctx context.Context, jobID int64) (string, error) // returns a URL or a command
	tmpl       *template.Template
	pollPeriod time.Duration
}

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	funcs := template.FuncMap{
		"money": func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		"short": func(s string, n int) string {
			if len(s) > n {
				return s[:n] + "…"
			}
			return s
		},
		"since": func(ts string) string {
			t, err := time.Parse(store.TimeFormat, ts)
			if err != nil {
				return ts
			}
			return time.Since(t).Round(time.Second).String() + " ago"
		},
		"repo": func() string { return s.Repo },
		"pretty": func(raw string) string {
			var v any
			if json.Unmarshal([]byte(raw), &v) != nil {
				return raw
			}
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
	}
	t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	if s.pollPeriod == 0 {
		s.pollPeriod = 500 * time.Millisecond
	}
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", s.needsYou)
	mux.HandleFunc("GET /board", s.board)
	mux.HandleFunc("GET /items/{id}", s.item)
	mux.HandleFunc("GET /jobs/{id}", s.job)
	mux.HandleFunc("GET /jobs/{id}/stream", s.stream)
	mux.HandleFunc("POST /jobs/{id}/retry", s.retry)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /jobs/{id}/attach", s.attach)
	mux.HandleFunc("POST /items/{id}/park", s.park)
	mux.HandleFunc("GET /orchestrator", s.orchestrator)
	mux.HandleFunc("GET /spend", s.spend)
	return sameOrigin(mux), nil
}

// sameOrigin rejects cross-site POSTs, so a page in your browser cannot
// press the control room's buttons.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data["Page"] = name
	if err := s.tmpl.ExecuteTemplate(w, name+".html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func fail(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func idParam(r *http.Request) (int64, error) { return strconv.ParseInt(r.PathValue("id"), 10, 64) }

// --- pages ---

type parkedRow struct {
	Item   *store.WorkItem
	Reason string
}

func (s *Server) needsYou(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	waiting, err := s.Store.ListItems(ctx, state.AwaitingHuman)
	if err != nil {
		fail(w, err)
		return
	}
	parkedItems, err := s.Store.ListItems(ctx, state.Parked)
	if err != nil {
		fail(w, err)
		return
	}
	var parked []parkedRow
	for _, it := range parkedItems {
		reason := ""
		if evs, err := s.Store.Events(ctx, it.ID); err == nil {
			for i := len(evs) - 1; i >= 0; i-- {
				if evs[i].Type == "stage" && strings.Contains(evs[i].Data, `"to":"parked"`) {
					reason = evs[i].Reason
					break
				}
			}
		}
		parked = append(parked, parkedRow{it, reason})
	}
	spent, _ := s.Store.SpendOn(ctx, store.Today())
	running, _ := s.Store.ListJobs(ctx, store.JobFilter{State: state.Running})
	s.render(w, "home", map[string]any{
		"Waiting": waiting, "Parked": parked, "Spent": spent, "Cap": s.DailyCap,
		"OverBudget": spent >= s.DailyCap, "NearBudget": spent >= 0.8*s.DailyCap,
		"AwaitFull": len(waiting) >= s.AwaitCap, "AwaitCap": s.AwaitCap, "Running": running,
	})
}

var boardStages = []state.Stage{state.Triage, state.Spec, state.Ready, state.Implementing, state.Reviewing, state.AwaitingHuman, state.Parked, state.Blocked, state.Done}

type boardCard struct {
	Item  *store.WorkItem
	Model string
	Cost  float64
	Live  bool
}

type boardCol struct {
	Stage state.Stage
	Cards []boardCard
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := s.Store.ListItems(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	cols := map[state.Stage]*boardCol{}
	for _, st := range boardStages {
		cols[st] = &boardCol{Stage: st}
	}
	for _, it := range items {
		jobs, _ := s.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		c := boardCard{Item: it}
		for _, j := range jobs {
			c.Cost += j.CostUSD
			if c.Model == "" && j.Kind == "implement" {
				c.Model = j.Model
			}
			c.Live = c.Live || j.State == state.Running
		}
		if col, ok := cols[it.Stage]; ok {
			col.Cards = append(col.Cards, c)
		}
	}
	var out []*boardCol
	for _, st := range boardStages {
		out = append(out, cols[st])
	}
	s.render(w, "board", map[string]any{"Cols": out})
}

func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, _ := s.Store.ListJobs(ctx, store.JobFilter{ItemID: id})
	evs, _ := s.Store.Events(ctx, id)
	deps, _ := s.Store.Dependencies(ctx, id)
	s.render(w, "item", map[string]any{"Item": it, "Jobs": jobs, "Events": evs, "Deps": deps})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	j, err := s.Store.GetJob(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	gr, _ := s.Store.GateResults(ctx, id)
	s.render(w, "job", map[string]any{"Job": j, "Gates": gr, "Live": j.State == state.Running, "Msg": r.URL.Query().Get("msg")})
}

// stream tails the job's log file over SSE until the job ends.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	var f *os.File
	var rd *bufio.Reader
	for {
		j, err := s.Store.GetJob(ctx, id)
		if err != nil {
			return
		}
		if f == nil && j.LogPath != "" {
			if f, err = os.Open(j.LogPath); err == nil {
				defer f.Close()
				rd = bufio.NewReaderSize(f, 1<<20)
			} else {
				f = nil
			}
		}
		if rd != nil {
			for {
				line, err := rd.ReadString('\n')
				if err == io.EOF {
					if line != "" {
						// partial line: rewind so it is re-read when complete
						_, _ = f.Seek(-int64(len(line)), io.SeekCurrent)
						rd.Reset(f)
					}
					break
				}
				if err != nil {
					return
				}
				fmt.Fprintf(w, "event: line\ndata: <div class=\"l\">%s</div>\n\n", template.HTMLEscapeString(summarize(line)))
			}
			fl.Flush()
		}
		if j.State.Terminal() {
			fmt.Fprintf(w, "event: done\ndata: <em>job %s</em>\n\n", j.State)
			fl.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.pollPeriod):
		}
	}
}

// summarize turns one raw CLI JSON line into a short human line.
func summarize(line string) string {
	var v struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &v) != nil {
		return strings.TrimSpace(line)
	}
	var parts []string
	for _, c := range v.Message.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "tool_use":
			parts = append(parts, "→ "+c.Name+" "+trim(string(c.Input), 160))
		}
	}
	switch {
	case v.Type == "result":
		parts = append(parts, "result ("+v.Subtype+"): "+trim(v.Result, 400))
	case v.Item.Type == "agent_message":
		parts = append(parts, v.Item.Text)
	case v.Item.Type == "command_execution":
		parts = append(parts, "→ "+v.Item.Command)
	}
	if len(parts) == 0 {
		return v.Type
	}
	return trim(strings.Join(parts, " "), 2000)
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?msg="+urlEscape(msg), http.StatusSeeOther)
}

func urlEscape(s string) string {
	return strings.NewReplacer(" ", "+", "&", "%26", "#", "%23", "?", "%3F").Replace(s)
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	msg := "Retry queued."
	if err := s.Actions.Retry(r.Context(), id, r.FormValue("tier") == "up"); err != nil {
		msg = "Retry refused: " + err.Error()
	}
	s.back(w, r, fmt.Sprintf("/jobs/%d", id), msg)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	msg := "Cancelled."
	if err := s.Actions.Cancel(r.Context(), id); err != nil {
		msg = "Cancel refused: " + err.Error()
	}
	s.back(w, r, fmt.Sprintf("/jobs/%d", id), msg)
}

func (s *Server) park(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		reason = "Parked from the control room."
	}
	msg := "Parked."
	if err := s.Actions.Park(r.Context(), id, reason); err != nil {
		msg = "Park refused: " + err.Error()
	}
	s.back(w, r, fmt.Sprintf("/items/%d", id), msg)
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.Attach == nil {
		s.back(w, r, fmt.Sprintf("/jobs/%d", id), "Attach is not available with this runner.")
		return
	}
	target, err := s.Attach(r.Context(), id)
	if err != nil {
		s.back(w, r, fmt.Sprintf("/jobs/%d", id), "Attach failed: "+err.Error())
		return
	}
	if strings.HasPrefix(target, "http") {
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	s.back(w, r, fmt.Sprintf("/jobs/%d", id), "Run this on the host: "+target)
}

func (s *Server) orchestrator(w http.ResponseWriter, r *http.Request) {
	evs, err := s.Store.EventsByActor(r.Context(), "orchestrator", 300)
	if err != nil {
		fail(w, err)
		return
	}
	s.render(w, "orchestrator", map[string]any{"Events": evs})
}

type daySpend struct {
	Day  string
	Cost float64
}

func (s *Server) spend(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var days []daySpend
	var week float64
	for i := 0; i < 7; i++ {
		d := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		c, _ := s.Store.SpendOn(ctx, d)
		week += c
		days = append(days, daySpend{d, c})
	}
	done, _ := s.Store.ListItems(ctx, state.Done)
	var doneCost float64
	byModel := map[string]float64{}
	for _, it := range done {
		jobs, _ := s.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID})
		for _, j := range jobs {
			doneCost += j.CostUSD
			byModel[j.Model] += j.CostUSD
		}
	}
	perPR := 0.0
	if len(done) > 0 {
		perPR = doneCost / float64(len(done))
	}
	type mc struct {
		Model string
		Cost  float64
	}
	var models []mc
	for m, c := range byModel {
		models = append(models, mc{m, c})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Cost > models[j].Cost })
	s.render(w, "spend", map[string]any{"Days": days, "Week": week, "Merged": len(done), "PerPR": perPR, "Models": models, "Cap": s.DailyCap})
}

// TTYDAttach returns an Attach func that opens a one-shot browser terminal
// into a running job's container with ttyd, when ttyd is installed, and
// otherwise returns the docker exec command to run by hand.
func TTYDAttach(st *store.Store, host string) func(ctx context.Context, jobID int64) (string, error) {
	return func(ctx context.Context, jobID int64) (string, error) {
		j, err := st.GetJob(ctx, jobID)
		if err != nil {
			return "", err
		}
		if j.State != state.Running {
			return "", fmt.Errorf("job %d is %s, not running", jobID, j.State)
		}
		name := fmt.Sprintf("factory-job-%d", jobID)
		manual := "docker exec -it " + name + " sh"
		if _, err := exec.LookPath("ttyd"); err != nil {
			return manual, nil
		}
		l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return manual, nil
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		cmd := exec.Command("ttyd", "--once", "--writable", "-i", host, "-p", strconv.Itoa(port), "docker", "exec", "-it", name, "sh")
		if err := cmd.Start(); err != nil {
			return manual, nil
		}
		go func() { _ = cmd.Wait() }()
		time.Sleep(300 * time.Millisecond)
		return fmt.Sprintf("http://%s/", net.JoinHostPort(host, strconv.Itoa(port))), nil
	}
}
