package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/preview"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// Previews: once a web app's change passes its gates, the factory runs
// the app from the checked export, in the job image like a gate (no
// secrets, the same network rules), and takes screenshots of the pages
// it touches. The reviewer looks at them, and the PR gets one comment
// with them, kept up to date on each push. Nothing here fails a build:
// what goes wrong becomes a note in the comment.

// Previewer is a forge client that can host a PR's screenshots and keep
// one comment on it up to date.
type Previewer interface {
	UploadPreview(ctx context.Context, n int, key string, files []github.File) ([]github.Upload, error)
	RemovePreview(ctx context.Context, n int, refs []string) error
	UpsertPRComment(ctx context.Context, n int, id int64, body string) (int64, error)
}

// capturePreview screenshots the change in gateDir, the export its gates
// passed on, and with before its base too, into the job's preview folder.
func (p *Pipeline) capturePreview(ctx context.Context, jobID int64, pt *part, gateDir string, pages []string) {
	cfg, ok := p.Cfg.PreviewFor(pt.repo)
	if !ok {
		return
	}
	dest := filepath.Join(p.jobDir(jobID), preview.Dir)
	m := &preview.Manifest{Head: pt.head, At: store.Now()}
	plans := preview.Plans(cfg, pages, p.Cfg.Previews.MaxScreenshots, p.Cfg.Previews.Timeout.Duration)
	for _, pl := range plans {
		if ctx.Err() != nil {
			return
		}
		dir, label := gateDir, ""
		if pl.Side == preview.Before {
			m.Base, label = pt.base, "Before: "
			var err error
			if dir, err = p.previewBase(ctx, jobID, pt); err != nil {
				m.Notes = append(m.Notes, label+"the base couldn't be set up: "+truncate(err.Error(), 500))
				m.Summary = append(m.Summary, label+"the base couldn't be set up")
				continue
			}
			defer os.RemoveAll(dir)
		}
		shots, notes, summary := p.captureSide(ctx, jobID, pt.repo, dir, pl, cfg.Ready, dest)
		m.Shots = append(m.Shots, shots...)
		for _, n := range notes {
			m.Notes = append(m.Notes, label+n)
		}
		for _, n := range summary {
			m.Summary = append(m.Summary, label+n)
		}
	}
	if len(plans) == 0 {
		m.Notes = append(m.Notes, "previews.max_screenshots leaves no room for one screenshot of each side")
		m.Summary = slices.Clone(m.Notes)
	}
	p.warn(m.Save(dest), "save preview")
	p.warn(p.Store.AddEvent(ctx, store.Event{Actor: "daemon", JobID: &jobID, Type: "preview",
		Reason: fmt.Sprintf("%d screenshots, %d notes", len(m.Shots), len(m.Notes))}), "AddEvent")
}

// previewBase exports the base, with the repo's files and dependencies,
// for its screenshots.
func (p *Pipeline) previewBase(ctx context.Context, jobID int64, pt *part) (string, error) {
	dir := filepath.Join(p.jobDir(jobID), "preview-base"+pt.sfx())
	if err := p.git(pt.repo).Export(ctx, pt.ws.Repo, pt.base, dir); err != nil {
		return "", err
	}
	if _, err := p.copyFilesInto(ctx, pt.repo, dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if install := p.repoConf(pt.repo).Gates.Install; install != "" {
		code, out, err := p.gateExec(ctx, jobID, dir, install, gateRun{repo: pt.repo, install: true})
		if err != nil || code != 0 {
			os.RemoveAll(dir)
			return "", fmt.Errorf("install: exit %d %v\n%s", code, err, tail(out, 500))
		}
	}
	return dir, nil
}

// captureSide runs one plan and collects what it took. notes say why
// each missing screenshot is missing, quoting the app and the browser,
// for the dashboard only: the app's log can hold the secrets in the
// repo's files. summary says it for the PR comment, in the factory's own
// words.
func (p *Pipeline) captureSide(ctx context.Context, jobID int64, repo, dir string, pl preview.Plan, ready, dest string) (shots []preview.Shot, notes, summary []string) {
	timeout := p.Cfg.Previews.Timeout.Duration
	code, out, err := p.gateExec(ctx, jobID, dir, pl.Command(), gateRun{repo: repo, timeout: timeout})
	switch {
	case errors.Is(err, runner.ErrTimeout):
		notes = append(notes, fmt.Sprintf("the capture timed out after %s (previews.timeout)", roundDur(timeout)))
		summary = slices.Clone(notes)
	case err != nil:
		notes = append(notes, "the capture couldn't run: "+truncate(err.Error(), 300))
		summary = []string{"the capture couldn't run"}
	case code == preview.ExitNoBrowser:
		notes = []string{"the job image has no Playwright: build it with --build-arg PLAYWRIGHT=1 (`factory images --force` does for a repo with preview)"}
		return nil, notes, slices.Clone(notes)
	case code == preview.ExitNotReady:
		return nil, []string{fmt.Sprintf("the app didn't answer %s on port %d within %s; the end of its log:\n%s", ready, pl.Port, roundDur(pl.Wait), tail(appLog(out), 800))},
			[]string{fmt.Sprintf("the app didn't answer %s within %s", ready, roundDur(pl.Wait))}
	case code != 0:
		notes = append(notes, fmt.Sprintf("the capture failed (exit %d):\n%s", code, tail(out, 800)))
		summary = []string{fmt.Sprintf("the capture failed (exit %d)", code)}
	}
	notes = append(notes, preview.Notes(out)...)
	shots, bad := preview.Collect(dir, pl, dest)
	if missing := len(pl.Targets) - len(shots); missing > 0 && len(summary) == 0 {
		summary = []string{fmt.Sprintf("%d of %d screenshots couldn't be taken", missing, len(pl.Targets))}
	}
	return shots, append(notes, bad...), summary
}

// appLog is the app's own output, from what the capture printed.
func appLog(out string) string {
	if _, after, ok := strings.Cut(out, "--- the app's log:\n"); ok {
		return after
	}
	return out
}

// reviewShots copies an implement job's screenshots into the review's
// output folder and lists them for the prompt, as the reviewer sees them.
func (p *Pipeline) reviewShots(implJob int64, out, seenOut string) []PromptShot {
	src := filepath.Join(p.jobDir(implJob), preview.Dir)
	m, err := preview.Load(src)
	if err != nil || m == nil {
		return nil
	}
	dst := filepath.Join(out, preview.Dir)
	var shots []PromptShot
	for _, s := range m.Shots {
		if len(shots) == maxReviewShots {
			break
		}
		b, err := os.ReadFile(filepath.Join(src, filepath.Base(s.File)))
		if err != nil || os.MkdirAll(dst, 0o755) != nil || os.WriteFile(filepath.Join(dst, filepath.Base(s.File)), b, 0o644) != nil {
			continue
		}
		shots = append(shots, PromptShot{Path: filepath.ToSlash(filepath.Join(seenOut, preview.Dir, filepath.Base(s.File))), Page: s.Page, Viewport: s.Viewport, Side: s.Side})
	}
	return shots
}

// maxReviewShots is as many images as a reviewer is shown.
const maxReviewShots = 12

// PromptShot is a screenshot the reviewer can look at.
type PromptShot struct{ Path, Page, Viewport, Side string }

// postPreview puts a build's screenshots on its PR: it uploads them, then
// writes them into the PR's one preview comment, then removes the last
// build's. Without screenshots the comment says why.
func (p *Pipeline) postPreview(ctx context.Context, it *store.WorkItem, pr *github.PR, in ReviewInput) {
	cfg, ok := p.Cfg.PreviewFor(it.Repo)
	if !ok || in.ImplementJob == 0 {
		return
	}
	fp, ok := p.gh(it.Repo).(Previewer)
	if !ok {
		return
	}
	dir := filepath.Join(p.jobDir(in.ImplementJob), preview.Dir)
	m, err := preview.Load(dir)
	if err != nil || m == nil {
		return // no capture ran for this build
	}
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	prev, err := p.Store.Preview(ctx, it.ID)
	if err != nil {
		p.warn(err, "preview")
		return
	}
	if prev == nil {
		prev = &store.Preview{ItemID: it.ID, Repo: it.Repo}
	}
	orig := *prev // as recorded, for when the comment can't be written
	if prev.PRNumber != pr.Number {
		prev.CommentID = 0 // a new PR gets its own comment
	}
	if prev.JobID == in.ImplementJob && prev.CommentID != 0 {
		return // posted already; this is a resumed PR step
	}
	var files []github.File
	for _, s := range m.Shots {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.Base(s.File))); err == nil {
			files = append(files, github.File{Name: filepath.Base(s.File), Data: b})
		}
	}
	var ups []github.Upload
	var upErr error
	if len(files) > 0 {
		ups, upErr = fp.UploadPreview(ctx, pr.Number, short(m.Head), files)
	}
	body := previewComment(m, ups, upErr, cfg.PreviewURL(pr.Number, p.branchFor(it), in.Head))
	refs := make([]string, 0, len(ups))
	for _, u := range ups {
		refs = append(refs, u.Ref)
	}
	// The last build's, but not one the new build just put at the same
	// place (the same commit).
	var old []string
	for _, r := range prev.Uploads {
		if !slices.Contains(refs, r) {
			old = append(old, r)
		}
	}
	id, err := fp.UpsertPRComment(ctx, pr.Number, prev.CommentID, body)
	if err != nil {
		// The comment still shows the last build's images: keep them,
		// and record the new ones too so they're removed in time. The
		// rest stays as recorded (the old PR's comment, the last post's
		// time), but a first post's uploads are this PR's.
		p.warn(err, "preview comment")
		keep := orig
		if keep.PRNumber == 0 {
			keep.PRNumber = pr.Number
		}
		keep.Uploads = append(slices.Clone(refs), old...)
		p.warn(p.Store.SetPreview(ctx, keep), "SetPreview")
		return
	}
	if len(old) > 0 {
		if err := fp.RemovePreview(ctx, prev.PRNumber, old); err != nil && !errors.Is(err, github.ErrNotRemovable) {
			p.warn(err, "remove old preview")
			refs = append(refs, old...) // tried again when these go
		}
	}
	p.warn(p.Store.SetPreview(ctx, store.Preview{ItemID: it.ID, Repo: it.Repo, PRNumber: pr.Number, CommentID: id, JobID: in.ImplementJob, Uploads: refs}), "SetPreview")
}

// previewComment is the PR comment: per page, a row per side, a column
// per viewport.
func previewComment(m *preview.Manifest, ups []github.Upload, upErr error, live string) string {
	var b strings.Builder
	b.WriteString(previewMarker + "\n### Screenshots\n\n")
	fmt.Fprintf(&b, "Taken by the factory on commit `%s` after its gates passed", short(m.Head))
	if m.Base != "" {
		fmt.Fprintf(&b, ", next to its base `%s`", short(m.Base))
	}
	b.WriteString(".")
	if live != "" {
		fmt.Fprintf(&b, " [Live preview](%s).", live)
	}
	b.WriteString("\n\n")
	url := map[string]string{}
	for _, u := range ups {
		url[u.Name] = u.URL
	}
	var pages, vps []string
	seen := map[string]bool{}
	for _, s := range m.Shots {
		if !seen["p"+s.Page] {
			seen["p"+s.Page] = true
			pages = append(pages, s.Page)
		}
		if !seen["v"+s.Viewport] {
			seen["v"+s.Viewport] = true
			vps = append(vps, s.Viewport)
		}
	}
	switch {
	case len(m.Shots) == 0:
		b.WriteString("**No screenshots this time.** This doesn't hold the PR up.\n\n")
	case upErr != nil || len(ups) == 0:
		b.WriteString("**The screenshots couldn't be uploaded** (they're on the item's page in the dashboard). This doesn't hold the PR up.\n\n")
	default:
		sides := []string{preview.After}
		if m.Base != "" {
			sides = []string{preview.Before, preview.After}
		}
		for _, pg := range pages {
			fmt.Fprintf(&b, "#### `%s`\n\n|  | %s |\n| --- |%s\n", pg, strings.Join(vps, " | "), strings.Repeat(" --- |", len(vps)))
			for _, side := range sides {
				fmt.Fprintf(&b, "| %s |", side)
				for _, vp := range vps {
					cell := " – |"
					for _, s := range m.Shots {
						if s.Page == pg && s.Viewport == vp && s.Side == side && url[s.File] != "" {
							cell = fmt.Sprintf(" [![%s %s](%s)](%s) |", side, vp, url[s.File], url[s.File])
						}
					}
					b.WriteString(cell)
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}
	// Never m.Notes: they quote the app's log.
	notes := m.Summary
	if upErr != nil {
		notes = append([]string{"upload: " + SafeText(upErr.Error(), 500)}, notes...)
	}
	if len(notes) > 0 {
		text := truncate(strings.Join(notes, "\n"), 4000)
		f := fence(text)
		fmt.Fprintf(&b, "<details><summary>What couldn't be captured</summary>\n\n%stext\n%s\n%s\n\nThe app's log and the details are on the item's page in the factory's dashboard.\n</details>\n\n", f, text, f)
	}
	b.WriteString("_Screenshots of the running app, not proof that it works. They're removed when the PR closes._\n")
	return b.String()
}

const previewMarker = "<!-- factory:preview -->"

// dropPreview removes a PR's screenshots, and says so in its comment.
func (p *Pipeline) dropPreview(ctx context.Context, it *store.WorkItem, why string) {
	rec, err := p.Store.Preview(ctx, it.ID)
	if err != nil || rec == nil || len(rec.Uploads) == 0 {
		return
	}
	p.removePreview(ctx, rec, why)
}

// removePreview removes what rec says is up, unless a build has posted
// since rec was read: then its uploads and comment are the new build's.
func (p *Pipeline) removePreview(ctx context.Context, rec *store.Preview, why string) {
	fp, ok := p.gh(rec.Repo).(Previewer)
	if !ok {
		return
	}
	p.previewMu.Lock()
	defer p.previewMu.Unlock()
	now, err := p.Store.Preview(ctx, rec.ItemID)
	if err != nil || now == nil || now.JobID != rec.JobID || now.PostedAt != rec.PostedAt || !slices.Equal(now.Uploads, rec.Uploads) {
		return
	}
	msg := "_The factory removed this PR's screenshots " + why + "._"
	if err := fp.RemovePreview(ctx, rec.PRNumber, rec.Uploads); errors.Is(err, github.ErrNotRemovable) {
		// For good: stop trying, and don't claim they're gone.
		msg = "_The factory couldn't remove this PR's screenshots " + why + ": " + SafeText(err.Error(), 300) + "._"
	} else if err != nil {
		p.warn(err, "remove preview")
		return // tried again on the next poll
	}
	rec.Uploads = nil
	p.warn(p.Store.SetPreview(ctx, *rec), "SetPreview")
	if rec.CommentID != 0 {
		_, err := fp.UpsertPRComment(ctx, rec.PRNumber, rec.CommentID, previewMarker+"\n### Screenshots\n\n"+msg+"\n")
		p.warn(err, "preview comment")
	}
}

// sweepPreviews removes screenshots uploaded longer ago than
// previews.keep_for, and those of a PR closed while its item was away
// from awaiting-you (closeOut sees the rest).
func (p *Pipeline) sweepPreviews(ctx context.Context, now time.Time) {
	ups, err := p.Store.PreviewsUp(ctx)
	if err != nil {
		p.warn(err, "previews")
		return
	}
	keep := p.Cfg.Previews.KeepFor.Duration
	for i := range ups {
		rec := &ups[i]
		if at, err := time.Parse(store.TimeFormat, rec.PostedAt); err == nil && now.Sub(at) >= keep {
			p.removePreview(ctx, rec, fmt.Sprintf("after %s (previews.keep_for)", roundDur(keep)))
			continue
		}
		if it, err := p.Store.GetItem(ctx, rec.ItemID); err != nil || it.Stage == state.AwaitingHuman {
			continue
		}
		if pr, err := p.gh(rec.Repo).GetPR(ctx, rec.PRNumber); err == nil && pr.State == "closed" {
			why := "once it was closed"
			if pr.Merged {
				why = "once it merged"
			}
			p.removePreview(ctx, rec, why)
		}
	}
}

// roundDur writes 1m0s as 1m and 336h as 14 days.
func roundDur(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}
