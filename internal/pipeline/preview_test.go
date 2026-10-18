package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/claptimes5/factory/internal/config"
	"github.com/claptimes5/factory/internal/github"
	"github.com/claptimes5/factory/internal/preview"
	"github.com/claptimes5/factory/internal/runner"
	"github.com/claptimes5/factory/internal/state"
	"github.com/claptimes5/factory/internal/store"
)

// previewGH is the fake forge with somewhere to put screenshots.
type previewGH struct {
	*fakeGH
	pmu     sync.Mutex
	uploads map[string]int   // ref -> PR
	notes   map[int64]string // the preview comments, by id
	edits   int
	nextID  int64

	upsertErr error // the comment can't be written
	removeErr error // the uploads can't be removed
	removes   int
}

func (g *previewGH) UploadPreview(_ context.Context, n int, key string, files []github.File) ([]github.Upload, error) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	var out []github.Upload
	for _, f := range files {
		ref := fmt.Sprintf("pr-%d/%s/%s", n, key, f.Name)
		g.uploads[ref] = n
		out = append(out, github.Upload{Name: f.Name, URL: "https://img.example/" + ref, Ref: ref})
	}
	return out, nil
}

func (g *previewGH) RemovePreview(_ context.Context, _ int, refs []string) error {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	g.removes++
	if g.removeErr != nil {
		return g.removeErr
	}
	for _, r := range refs {
		delete(g.uploads, r)
	}
	return nil
}

func (g *previewGH) UpsertPRComment(_ context.Context, _ int, id int64, body string) (int64, error) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	if g.upsertErr != nil {
		return 0, g.upsertErr
	}
	if _, ok := g.notes[id]; ok {
		g.notes[id] = body
		g.edits++
		return id, nil
	}
	g.nextID++
	g.notes[g.nextID] = body
	return g.nextID, nil
}

func (g *previewGH) note() string {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return g.notes[g.nextID]
}

// capture is how the fake job image answers a preview's command.
type capture struct {
	mode string // ready, not-ready, timeout or broken
	dirs []string
}

var planArg = regexp.MustCompile(`FACTORY_PLAN=([A-Za-z0-9+/=]+)`)

func shotPNG() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 64, 48)))
	return b.Bytes()
}

// withPreviews turns on the repo's preview and fakes the browser.
func (e *env) withPreviews(p config.Preview) (*previewGH, *capture) {
	pg := &previewGH{fakeGH: e.gh, uploads: map[string]int{}, notes: map[int64]string{}}
	e.p.GH = pg
	e.p.Cfg.Repos[0].Preview = &p
	c := &capture{mode: "ready"}
	inner := e.p.GateExec
	e.p.GateExec = func(ctx context.Context, workdir, command string) (int, string, error) {
		m := planArg.FindStringSubmatch(command)
		if m == nil {
			return inner(ctx, workdir, command)
		}
		c.dirs = append(c.dirs, workdir)
		var pl preview.Plan
		b, _ := base64.StdEncoding.DecodeString(m[1])
		json.Unmarshal(b, &pl)
		out := filepath.Join(workdir, pl.Out)
		os.MkdirAll(out, 0o755)
		switch c.mode {
		case "not-ready":
			return preview.ExitNotReady, "factory-preview: not ready: ECONNREFUSED\n--- the app's log:\nError: Cannot find module 'next'\n", nil
		case "timeout":
			return -1, "", runner.ErrTimeout
		case "broken":
			for i, t := range pl.Targets {
				data := shotPNG()
				if i == 0 {
					data = []byte("<html>")
				}
				os.WriteFile(filepath.Join(out, t.File), data, 0o644)
			}
			return 0, "factory-preview: /missing at phone: net::ERR_ABORTED\n", nil
		}
		for _, t := range pl.Targets {
			os.WriteFile(filepath.Join(out, t.File), shotPNG(), 0o644)
		}
		return 0, "", nil
	}
	return pg, c
}

// A web app's change gets screenshots of its pages, the configured ones
// and the builder's, before and after, at desktop and phone sizes: the
// reviewer looks at them and the PR gets them in one comment.
func TestPreviewPostsScreenshots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, c := e.withPreviews(config.Preview{Start: "npm start", Before: true})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.claude.implVerdict = `{"status":"done","summary":"did it","tests_added":["TestAdd"],"blocker":"","pages":["/settings","http://evil.example/","/","/billing","/more"]}`
	e.codex.reviews = []string{pass}
	if err := e.p.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, 1)
	if it.Stage != state.AwaitingHuman {
		t.Fatalf("stage = %s", it.Stage)
	}
	if !strings.Contains(e.claude.prompts["implement"][0], `"pages"`) {
		t.Fatal("the builder wasn't asked for the pages it changed")
	}
	if len(c.dirs) != 2 || !strings.HasSuffix(c.dirs[0], "/gate") || !strings.HasSuffix(c.dirs[1], "/preview-base") {
		t.Fatalf("captured in %q", c.dirs)
	}
	// 12 at most, 6 a side: /, /settings and /billing at two sizes.
	jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
	m, _ := preview.Load(filepath.Join(e.p.jobDir(jobs[0].ID), preview.Dir))
	if m == nil || len(m.Shots) != 12 || m.Base == "" || len(m.Notes) != 0 {
		t.Fatalf("manifest %+v", m)
	}
	review := e.codex.prompts["review"][0]
	for _, want := range []string{"## Screenshots", "`/billing` at phone size, with the change", "before the change (the base branch)"} {
		if !strings.Contains(review, want) {
			t.Fatalf("review prompt missing %q:\n%s", want, review)
		}
	}
	// The reviewer gets them as files in its output folder.
	if ims := e.codex.images[0]; len(ims) != 12 || !strings.Contains(ims[0], "/out/preview/") {
		t.Fatalf("reviewer's images %q", ims)
	}
	body := pg.note()
	for _, want := range []string{previewMarker, "#### `/settings`", "| before |", "| after |", "](https://img.example/pr-", "| desktop | phone |"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "evil") || len(pg.uploads) != 12 {
		t.Fatalf("uploads %d, comment:\n%s", len(pg.uploads), body)
	}
	if rec, _ := e.p.Store.Preview(ctx, it.ID); rec == nil || rec.CommentID != pg.nextID || len(rec.Uploads) != 12 || rec.PRNumber != it.PRNumber {
		t.Fatalf("record %+v", rec)
	}
}

// When the app can't be screenshotted, the build goes on and the PR says
// so in the same comment, in the factory's words: the app's log and the
// browser's errors stay on the dashboard, since the log can quote the
// secrets in the repo's files.
func TestPreviewFailureNeverFailsTheBuild(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string]struct{ comment, notes []string }{
		"not-ready": {[]string{"the app didn't answer / within 1m"}, []string{"Error: Cannot find module 'next'", "on port 3000"}},
		"timeout":   {[]string{"the capture timed out after 5m (previews.timeout)"}, []string{"timed out after 5m"}},
		"broken":    {[]string{"1 of 2 screenshots couldn't be taken"}, []string{"/ at desktop: not a PNG", "net::ERR_ABORTED"}},
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			e := setup(t)
			pg, c := e.withPreviews(config.Preview{Start: "npm start"})
			c.mode = mode
			e.addIssue(1)
			e.claude.builds = []func(string) error{goodBuild}
			e.codex.reviews = []string{pass}
			if err := e.p.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			it := e.item(t, 1)
			if it.Stage != state.AwaitingHuman || e.claude.calls["implement"] != 1 {
				t.Fatalf("stage %s after %d builds", it.Stage, e.claude.calls["implement"])
			}
			body := pg.note()
			for _, w := range append(want.comment, "What couldn't be captured", "on the item's page in the factory's dashboard") {
				if !strings.Contains(body, w) {
					t.Fatalf("comment missing %q:\n%s", w, body)
				}
			}
			for _, leak := range []string{"Cannot find module", "ERR_ABORTED", "not a PNG", "port 3000"} {
				if strings.Contains(body, leak) {
					t.Fatalf("comment quotes the capture's output (%q):\n%s", leak, body)
				}
			}
			jobs, _ := e.p.Store.ListJobs(ctx, store.JobFilter{ItemID: it.ID, Kind: "implement"})
			m, _ := preview.Load(filepath.Join(e.p.jobDir(jobs[0].ID), preview.Dir))
			notes := strings.Join(m.Notes, "\n")
			for _, w := range want.notes {
				if !strings.Contains(notes, w) {
					t.Fatalf("dashboard notes missing %q:\n%s", w, notes)
				}
			}
			if mode == "broken" {
				if len(pg.uploads) != 1 {
					t.Fatalf("uploads %v:\n%s", pg.uploads, body)
				}
				return
			}
			if !strings.Contains(body, "No screenshots this time") || strings.Contains(e.codex.prompts["review"][0], "## Screenshots") {
				t.Fatalf("comment:\n%s", body)
			}
		})
	}
}

// Each build edits the PR's one comment, and the last build's images go
// once the new ones are up.
func TestPreviewCommentUpdatedInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	first := len(pg.uploads)
	if first != 2 || len(pg.notes) != 1 {
		t.Fatalf("first build: %d uploads, %d comments", first, len(pg.notes))
	}
	// A second build of the same PR.
	dir := filepath.Join(e.p.jobDir(999), preview.Dir)
	(&preview.Manifest{Head: "c0ffee0000", Shots: []preview.Shot{{File: "after-01-desktop.png", Page: "/", Viewport: "desktop", Side: preview.After}}}).Save(dir)
	os.WriteFile(filepath.Join(dir, "after-01-desktop.png"), shotPNG(), 0o644)
	pr, _ := e.gh.GetPR(ctx, it.PRNumber)
	in := ReviewInput{ImplementJob: 999, Head: "c0ffee0000"}
	e.p.postPreview(ctx, it, pr, in)
	if len(pg.notes) != 1 || pg.edits != 1 || !strings.Contains(pg.note(), "`c0ffee0000`") {
		t.Fatalf("%d comments, %d edits:\n%s", len(pg.notes), pg.edits, pg.note())
	}
	if len(pg.uploads) != 1 {
		t.Fatalf("the last build's images stayed: %v", pg.uploads)
	}
	for ref := range pg.uploads {
		if !strings.Contains(ref, "/c0ffee0000/") {
			t.Fatalf("left %s", ref)
		}
	}
	// A resumed PR step posts nothing again.
	e.p.postPreview(ctx, it, pr, in)
	if pg.edits != 1 {
		t.Fatalf("posted twice: %d edits", pg.edits)
	}
}

// A merged PR's screenshots are removed, and its comment says so.
func TestPreviewRemovedWhenThePRMerges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	if len(pg.uploads) == 0 {
		t.Fatal("nothing uploaded")
	}
	e.gh.prs["factory/issue-1"].Merged = true
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	e.p.Drain(ctx)
	if len(pg.uploads) != 0 || !strings.Contains(pg.note(), "removed this PR's screenshots once it merged") {
		t.Fatalf("uploads %v, comment:\n%s", pg.uploads, pg.note())
	}
	if up, _ := e.p.Store.PreviewsUp(ctx); len(up) != 0 {
		t.Fatalf("still recorded as up: %+v", up)
	}
}

// Screenshots of a PR left open go after previews.keep_for.
func TestPreviewKeepFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	e.p.sweepPreviews(ctx, time.Now().Add(13*24*time.Hour))
	if len(pg.uploads) == 0 {
		t.Fatal("removed before keep_for")
	}
	e.p.sweepPreviews(ctx, time.Now().Add(15*24*time.Hour))
	if len(pg.uploads) != 0 || !strings.Contains(pg.note(), "after 14 days (previews.keep_for)") {
		t.Fatalf("uploads %v, comment:\n%s", pg.uploads, pg.note())
	}
}

// Without the setting nothing is captured, and a forge with nowhere to
// put screenshots still gets a build.
func TestPreviewOffAndUnsupportedForge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	_, c := e.withPreviews(config.Preview{Start: "npm start"})
	e.p.Cfg.Repos[0].Preview = nil
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild, goodBuild}
	e.codex.reviews = []string{pass, pass}
	e.p.Drain(ctx)
	if len(c.dirs) != 0 || strings.Contains(e.claude.prompts["implement"][0], `"pages"`) {
		t.Fatalf("captured without a preview setting: %q", c.dirs)
	}
	e.p.Cfg.Repos[0].Preview = &config.Preview{Start: "npm start"}
	e.p.GH = e.gh // can't host images
	e.addIssue(2)
	e.p.lastPoll = e.p.lastPoll.AddDate(0, 0, -1)
	e.p.Drain(ctx)
	if it := e.item(t, 2); it.Stage != state.AwaitingHuman || len(c.dirs) != 1 {
		t.Fatalf("stage %s, captures %d", it.Stage, len(c.dirs))
	}
}

// secondBuild puts a build 999 of commit c0ffee0000 in place, with one
// screenshot, ready to post.
func (e *env) secondBuild(t *testing.T) ReviewInput {
	t.Helper()
	dir := filepath.Join(e.p.jobDir(999), preview.Dir)
	(&preview.Manifest{Head: "c0ffee0000", Shots: []preview.Shot{{File: "after-01-desktop.png", Page: "/", Viewport: "desktop", Side: preview.After}}}).Save(dir)
	os.WriteFile(filepath.Join(dir, "after-01-desktop.png"), shotPNG(), 0o644)
	return ReviewInput{ImplementJob: 999, Head: "c0ffee0000"}
}

// When the comment can't be written it still shows the last build's
// images, so they stay up, and the new ones are recorded to go in time.
func TestPreviewKeptWhenTheCommentFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	first, _ := e.p.Store.Preview(ctx, it.ID)
	pr, _ := e.gh.GetPR(ctx, it.PRNumber)
	pg.upsertErr = fmt.Errorf("502 Bad Gateway")
	e.p.postPreview(ctx, it, pr, e.secondBuild(t))
	if len(pg.uploads) != 3 {
		t.Fatalf("the shown images went with the comment failing: %v", pg.uploads)
	}
	rec, _ := e.p.Store.Preview(ctx, it.ID)
	if len(rec.Uploads) != 3 || rec.JobID != first.JobID || rec.CommentID != first.CommentID {
		t.Fatalf("record %+v after %+v", rec, first)
	}
	// The next post goes through and removes all the earlier ones.
	pg.upsertErr = nil
	e.p.postPreview(ctx, it, pr, e.secondBuild(t))
	if len(pg.uploads) != 1 {
		t.Fatalf("left %v", pg.uploads)
	}
}

// A sweep that read the record before a build posted leaves the new
// build's screenshots and comment alone.
func TestPreviewSweepDoesntRemoveANewerPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	stale, _ := e.p.Store.Preview(ctx, it.ID)
	pr, _ := e.gh.GetPR(ctx, it.PRNumber)
	e.p.postPreview(ctx, it, pr, e.secondBuild(t))
	e.p.removePreview(ctx, stale, "after 14 days (previews.keep_for)")
	if len(pg.uploads) != 1 || strings.Contains(pg.note(), "factory removed") || !strings.Contains(pg.note(), "c0ffee0000") {
		t.Fatalf("uploads %v, comment:\n%s", pg.uploads, pg.note())
	}
	if rec, _ := e.p.Store.Preview(ctx, it.ID); rec.JobID != 999 || len(rec.Uploads) != 1 {
		t.Fatalf("record %+v", rec)
	}
}

// A forge that won't remove them (GitLab below 17.2, or without the
// Maintainer role) is told once: the comment says so, without claiming
// they're gone, and the factory stops trying.
func TestPreviewNotRemovable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	pg.removeErr = fmt.Errorf("%w: deleting uploads takes the Maintainer role", github.ErrNotRemovable)
	e.p.sweepPreviews(ctx, time.Now().Add(15*24*time.Hour))
	body := pg.note()
	if !strings.Contains(body, "couldn't remove this PR's screenshots after 14 days") || !strings.Contains(body, "Maintainer") || strings.Contains(body, "removed this") {
		t.Fatalf("comment:\n%s", body)
	}
	if up, _ := e.p.Store.PreviewsUp(ctx); len(up) != 0 {
		t.Fatalf("still recorded as up: %+v", up)
	}
	e.p.sweepPreviews(ctx, time.Now().Add(16*24*time.Hour))
	if pg.removes != 1 {
		t.Fatalf("tried %d times", pg.removes)
	}
}

// A PR closed while its item is back in a fix round loses its
// screenshots on the next sweep.
func TestPreviewRemovedWhenClosedDuringAFix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	if err := e.p.Store.SetStage(ctx, it.ID, state.Implementing, "test", "a fix round"); err != nil {
		t.Fatal(err)
	}
	e.p.sweepPreviews(ctx, time.Now())
	if len(pg.uploads) == 0 {
		t.Fatal("removed while the PR is open")
	}
	e.gh.prs["factory/issue-1"].State = "closed"
	e.p.sweepPreviews(ctx, time.Now())
	if len(pg.uploads) != 0 || !strings.Contains(pg.note(), "removed this PR's screenshots once it was closed") {
		t.Fatalf("uploads %v, comment:\n%s", pg.uploads, pg.note())
	}
}

// A cut note stays valid UTF-8: a PR comment or a log line with half a
// character in it is mangled.
func TestTruncateKeepsRunesWhole(t *testing.T) {
	t.Parallel()
	for n := 0; n <= 5; n++ {
		if got := truncate("a€b", n); !utf8.ValidString(got) {
			t.Fatalf("truncate(%d) = %q", n, got)
		}
	}
	if got := truncate("a€b", 2); got != "a…" {
		t.Fatalf("truncate = %q", got)
	}
}

// A failed comment keeps the record as it was, so nothing is lost: a
// first post's uploads are recorded on its PR (never PR 0), and a new
// PR's failed post keeps the old PR's comment and the last post's time.
func TestPreviewFailedCommentKeepsTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := setup(t)
	pg, _ := e.withPreviews(config.Preview{Start: "npm start"})
	pg.upsertErr = fmt.Errorf("502 Bad Gateway")
	e.addIssue(1)
	e.claude.builds = []func(string) error{goodBuild}
	e.codex.reviews = []string{pass}
	e.p.Drain(ctx)
	it := e.item(t, 1)
	rec, _ := e.p.Store.Preview(ctx, it.ID)
	if rec == nil || rec.PRNumber != it.PRNumber || rec.PRNumber == 0 || rec.CommentID != 0 || len(rec.Uploads) != 2 {
		t.Fatalf("first post: record %+v", rec)
	}
	// Posted on PR 101 once; now a build for another PR fails to comment.
	pg.upsertErr = nil
	pr, _ := e.gh.GetPR(ctx, it.PRNumber)
	e.p.postPreview(ctx, it, pr, e.secondBuild(t))
	before, _ := e.p.Store.Preview(ctx, it.ID)
	if before.CommentID == 0 {
		t.Fatalf("not posted: %+v", before)
	}
	time.Sleep(10 * time.Millisecond) // a later posted_at, were it rewritten
	pg.upsertErr = fmt.Errorf("502 Bad Gateway")
	other := *pr
	other.Number = 202
	in := e.secondBuild(t)
	in.ImplementJob = 998
	dir := filepath.Join(e.p.jobDir(998), preview.Dir)
	(&preview.Manifest{Head: "beef000000", Shots: []preview.Shot{{File: "after-01-desktop.png", Page: "/", Viewport: "desktop", Side: preview.After}}}).Save(dir)
	os.WriteFile(filepath.Join(dir, "after-01-desktop.png"), shotPNG(), 0o644)
	e.p.postPreview(ctx, it, &other, in)
	after, _ := e.p.Store.Preview(ctx, it.ID)
	if after.PRNumber != before.PRNumber || after.CommentID != before.CommentID || after.JobID != before.JobID || after.PostedAt != before.PostedAt || len(after.Uploads) != 2 {
		t.Fatalf("record %+v, was %+v", after, before)
	}
}
