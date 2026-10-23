package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Files a person attaches to a chat message or to a message for a running
// build are saved as they came, under names made safe, and the agent is
// told where to read them. Images also go to a CLI that attaches pictures
// to its prompt (Codex).

// Limits on what one message carries.
const (
	maxAttachments  = 10
	maxAttachBytes  = 20 << 20 // all of a message's files together
	attachDir       = "files"  // in a run's out folder
	attachNameRunes = 100
)

// imageExts are the pictures a model can look at.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// IsImage says whether an attachment's name is a picture a model can see.
func IsImage(name string) bool { return imageExts[strings.ToLower(filepath.Ext(name))] }

// checkAttachments refuses too many or too big files.
func checkAttachments(files []store.Attachment) error {
	if len(files) > maxAttachments {
		return fmt.Errorf("that's %d files; attach at most %d to one message", len(files), maxAttachments)
	}
	n := 0
	for _, f := range files {
		n += len(f.Data)
	}
	if n > maxAttachBytes {
		return fmt.Errorf("the files come to %.1f MB; the most one message carries is %d MB", float64(n)/(1<<20), maxAttachBytes>>20)
	}
	return nil
}

// attachName makes a person's file name safe to write: no folders, no
// leading dot, no control characters, and unique among taken.
func attachName(name string, taken map[string]bool) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, path.Base(strings.ReplaceAll(name, "\\", "/")))
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	if name == "" {
		name = "file"
	}
	if r := []rune(name); len(r) > attachNameRunes {
		ext := filepath.Ext(name)
		if len([]rune(ext)) > 10 {
			ext = ""
		}
		name = string(r[:attachNameRunes-len([]rune(ext))]) + ext
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; taken[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s-%d%s", base, i, ext)
	}
	taken[strings.ToLower(name)] = true
	return name
}

// saveAttachments writes files into dir and returns the names they got.
func saveAttachments(dir string, files []store.Attachment) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	if es, err := os.ReadDir(dir); err == nil {
		for _, e := range es {
			taken[strings.ToLower(e.Name())] = true
		}
	}
	var names []string
	for _, f := range files {
		n := attachName(f.Name, taken)
		if err := os.WriteFile(filepath.Join(dir, n), f.Data, 0o644); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, nil
}

// listAttachments names the files in dir, in name order; none when it
// doesn't exist.
func listAttachments(dir string) []string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	return out
}

// attachedNote tells the agent where a message's files are, as it sees
// them.
func attachedNote(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The person attached these files (read each one; images too, with the tool that reads files):\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	return b.String()
}

// sweepAttachments removes attached files no one needs any more: a chat's
// once it has been quiet for orchestrator.keep_files_for, and a job's once
// it ended and its item is done or gone, it had no item (a chat reply's
// copies), or it ended that long ago.
func (p *Pipeline) sweepAttachments(ctx context.Context, now time.Time) {
	keep := p.Cfg.Orchestrator.KeepFilesFor.Duration
	old := func(at string) bool {
		t, err := time.Parse(store.TimeFormat, at)
		return err == nil && now.Sub(t) >= keep
	}
	data := p.Cfg.Path(p.Cfg.DataDir)
	chats, _ := filepath.Glob(filepath.Join(data, "chats", "*"))
	for _, d := range chats {
		id, err := strconv.ParseInt(filepath.Base(d), 10, 64)
		if err != nil {
			continue
		}
		// Only a definite answer removes files: a store error keeps them.
		c, err := p.Store.GetChat(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && old(c.UpdatedAt)) {
			p.warn(os.RemoveAll(d), "remove chat files")
		}
	}
	jobs, _ := filepath.Glob(filepath.Join(data, "jobs", "*", "out", attachDir))
	for _, d := range jobs {
		id, err := strconv.ParseInt(filepath.Base(filepath.Dir(filepath.Dir(d))), 10, 64)
		if err != nil {
			continue
		}
		if why := p.jobFilesStale(ctx, id, old); why != "" {
			p.log().Info("removing files sent to a job", "job", id, "why", why)
			p.warn(os.RemoveAll(d), "remove job files")
		}
	}
}

// jobFilesStale says why the files sent to job id should go, or "".
func (p *Pipeline) jobFilesStale(ctx context.Context, id int64, old func(string) bool) string {
	j, err := p.Store.GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "its job is gone"
	}
	if err != nil || !j.State.Terminal() {
		return ""
	}
	if j.ItemID == nil {
		return "its job has no item"
	}
	if old(j.FinishedAt) {
		return "it ended over orchestrator.keep_files_for ago"
	}
	it, err := p.Store.GetItem(ctx, *j.ItemID)
	if errors.Is(err, sql.ErrNoRows) {
		return "its item is gone"
	}
	if err == nil && it.Stage == state.Done {
		return "its item is done"
	}
	return ""
}
