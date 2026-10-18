package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/claptimes5/factory/internal/state"
)

// openTest opens a store copied from one migrated once per test binary:
// migrating runs every migration and backs up before each, most of a
// second under -race.
func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	tmplOnce.Do(func() {
		dir, err := os.MkdirTemp("", "store-template-")
		if err != nil {
			tmplErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "factory.db")
		s, err := Open(path)
		if err != nil {
			tmplErr = err
			return
		}
		// Closing the last connection checkpoints the WAL into the file.
		if tmplErr = s.Close(); tmplErr != nil {
			return
		}
		if _, err := os.Stat(path + "-wal"); err == nil {
			tmplErr = errors.New("the template's WAL wasn't checkpointed on close")
			return
		}
		tmpl, tmplErr = os.ReadFile(path)
	})
	if tmplErr != nil {
		t.Fatalf("migrating the template: %v", tmplErr)
	}
	path := filepath.Join(t.TempDir(), "factory.db")
	if err := os.WriteFile(path, tmpl, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

var (
	tmplOnce sync.Once
	tmpl     []byte
	tmplErr  error
)

// openFresh opens a new store, running every migration.
func openFresh(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "factory.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func newItem(t *testing.T, s *Store, n int) *WorkItem {
	t.Helper()
	it, _, err := s.UpsertItem(context.Background(), "o/r", n, "issue")
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func enqueue(t *testing.T, s *Store, itemID int64, prio int) int64 {
	t.Helper()
	id, err := s.EnqueueJob(context.Background(), Job{
		ItemID: &itemID, Kind: "implement", Harness: "claude", Model: "sonnet",
		BudgetUSD: 2, Priority: prio,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Many goroutines and two separate connections race to claim; every job must
// be claimed exactly once, and no claim may return a job twice.
func TestClaimRace(t *testing.T) {
	t.Parallel()
	s, path := openTest(t)
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	it := newItem(t, s, 1)
	const n = 60
	for i := 0; i < n; i++ {
		enqueue(t, s, it.ID, 0)
	}

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		st := s
		if w%2 == 1 {
			st = s2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := st.ClaimJob(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				if j.State != state.Running || j.StartedAt == "" {
					t.Errorf("claimed job %d has state %s started %q", j.ID, j.State, j.StartedAt)
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %d claimed %d times", id, c)
		}
	}
}

func TestClaimOrder(t *testing.T) {
	t.Parallel()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	low := enqueue(t, s, it.ID, 0)
	time.Sleep(time.Millisecond)
	high := enqueue(t, s, it.ID, 5)
	time.Sleep(time.Millisecond)
	low2 := enqueue(t, s, it.ID, 0)

	var got []int64
	for {
		j, err := s.ClaimJob(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		got = append(got, j.ID)
	}
	want := []int64{high, low, low2}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestFinishJobTransitionsAndSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	id := enqueue(t, s, it.ID, 0)

	// queued -> succeeded skips running and must be refused.
	if err := s.FinishJob(ctx, id, Result{State: state.Succeeded}); err == nil {
		t.Fatal("finishing a queued job should fail")
	}
	if _, err := s.ClaimJob(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, id, Result{State: state.Succeeded, CostUSD: 1.25, TokensIn: 10, TokensOut: 5, Verdict: `{"ok":true}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, id, Result{State: state.Failed}); err == nil {
		t.Fatal("a terminal job must not transition again")
	}
	j, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != state.Succeeded || j.CostUSD != 1.25 || j.FinishedAt == "" {
		t.Fatalf("unexpected job: %+v", j)
	}
	spent, err := s.SpendOn(ctx, Today())
	if err != nil {
		t.Fatal(err)
	}
	if spent != 1.25 {
		t.Fatalf("spend = %v, want 1.25", spent)
	}
}

func TestSetStageValidatesAndAudits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 7)
	if it.Stage != state.Triage {
		t.Fatalf("new item stage = %s", it.Stage)
	}
	if err := s.SetStage(ctx, it.ID, state.AwaitingHuman, "daemon", "skip"); err == nil {
		t.Fatal("triage -> awaiting_human must be refused")
	}
	if err := s.SetStage(ctx, it.ID, state.Ready, "daemon", "triaged"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetItem(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != state.Ready {
		t.Fatalf("stage = %s", got.Stage)
	}
	evs, err := s.Events(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evs {
		if e.Type == "stage" && e.Reason == "triaged" && e.Actor == "daemon" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no stage event recorded: %+v", evs)
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	a, created, err := s.UpsertItem(ctx, "o/r", 3, "first")
	if err != nil || !created {
		t.Fatalf("first upsert: %v created=%v", err, created)
	}
	b, created, err := s.UpsertItem(ctx, "o/r", 3, "renamed")
	if err != nil || created {
		t.Fatalf("second upsert: %v created=%v", err, created)
	}
	if a.ID != b.ID {
		t.Fatalf("ids differ: %d %d", a.ID, b.ID)
	}
}

func TestRequeueRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	a := enqueue(t, s, it.ID, 0)
	b := enqueue(t, s, it.ID, 0)
	s.ClaimJob(ctx)
	s.ClaimJob(ctx)
	// a's container is still alive, b's is gone.
	n, err := s.RequeueRunning(ctx, func(j *Job) bool { return j.ID == a })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("requeued %d, want 1", n)
	}
	ja, _ := s.GetJob(ctx, a)
	jb, _ := s.GetJob(ctx, b)
	if ja.State != state.Running || jb.State != state.Queued {
		t.Fatalf("a=%s b=%s", ja.State, jb.State)
	}
	if jb.Attempt != 1 {
		t.Fatalf("requeue must not use a retry: attempt=%d", jb.Attempt)
	}
}

func TestMigrationsAreForwardOnlyAndBackedUp(t *testing.T) {
	t.Parallel()
	s, path := openFresh(t)
	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != len(migrationFiles(t)) {
		t.Fatalf("schema version %d, want %d", v, len(migrationFiles(t)))
	}
	s.Close()
	// Reopening applies nothing and keeps the data.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v2, _ := s2.SchemaVersion(context.Background())
	if v2 != v {
		t.Fatalf("version changed on reopen: %d -> %d", v, v2)
	}
}

// A new database has nothing to back up before its migrations.
func TestNewDatabaseMakesNoBackups(t *testing.T) {
	t.Parallel()
	_, path := openFresh(t)
	if baks, _ := filepath.Glob(path + ".bak-*"); len(baks) != 0 {
		t.Fatalf("a new database was backed up: %v", baks)
	}
}

// factory backup writes a database that opens and holds the data.
func TestBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, path := openTest(t)
	it := newItem(t, s, 7)
	dst := filepath.Join(filepath.Dir(path), "backup.db")
	if err := s.Backup(ctx, dst); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := s.Backup(ctx, dst); err == nil {
		t.Fatal("a backup over an existing file must be refused")
	}
	b, err := Open(dst)
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer b.Close()
	if v, _ := b.SchemaVersion(ctx); v != len(migrationFiles(t)) {
		t.Fatalf("backup schema version %d", v)
	}
	got, err := b.ItemByIssue(ctx, "o/r", 7)
	if err != nil || got.ID != it.ID {
		t.Fatalf("backup item %+v %v", got, err)
	}
}

func migrationFiles(t *testing.T) []string {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.name
	}
	return out
}

func TestDependencyCyclesRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	a, b, c := newItem(t, s, 1), newItem(t, s, 2), newItem(t, s, 3)
	if err := s.SetDependencies(ctx, b.ID, []int64{a.ID}, "orchestrator", "b after a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDependencies(ctx, c.ID, []int64{b.ID}, "orchestrator", "c after b"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDependencies(ctx, a.ID, []int64{c.ID}, "orchestrator", "loop"); !errors.Is(err, ErrCycle) {
		t.Fatalf("want cycle error, got %v", err)
	}
	if err := s.SetDependencies(ctx, a.ID, []int64{a.ID}, "orchestrator", "self"); !errors.Is(err, ErrCycle) {
		t.Fatalf("self dependency must be refused, got %v", err)
	}
	deps, _ := s.Dependencies(ctx, c.ID)
	if len(deps) != 1 || deps[0] != b.ID {
		t.Fatalf("deps = %v", deps)
	}
}

func TestVersionMovesOnItemOnlyWrites(t *testing.T) {
	t.Parallel()
	st, _ := openTest(t)
	ctx := context.Background()
	it, _, _ := st.UpsertItem(ctx, "o/r", 1, "x")
	v1, err := st.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := st.SetPriority(ctx, it.ID, 5); err != nil {
		t.Fatal(err)
	}
	if v2, _ := st.Version(ctx); v2 == v1 {
		t.Fatal("a priority change must move the version")
	}
}

// A workspace job's heads and an item's PRs keep their order, and each
// write replaces the last.
func TestWorkspaceHeadsAndPRs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openTest(t)
	it := newItem(t, s, 1)
	id := enqueue(t, s, it.ID, 0)
	for _, hs := range [][]RepoHead{{{"o/web", "b"}, {"o/api", "a"}}, {{"o/web", "c"}}} {
		if err := s.SetJobHeads(ctx, id, hs); err != nil {
			t.Fatal(err)
		}
	}
	if hs, err := s.JobHeads(ctx, id); err != nil || len(hs) != 1 || hs[0] != (RepoHead{"o/web", "c"}) {
		t.Fatalf("heads = %+v %v", hs, err)
	}
	if err := s.SetItemPRs(ctx, it.ID, []RepoPR{{"o/web", 7}, {"o/api", 3}}); err != nil {
		t.Fatal(err)
	}
	if prs, err := s.ItemPRs(ctx, it.ID); err != nil || len(prs) != 2 || prs[0] != (RepoPR{"o/web", 7}) || prs[1] != (RepoPR{"o/api", 3}) {
		t.Fatalf("prs = %+v %v", prs, err)
	}
	if prs, _ := s.ItemPRs(ctx, it.ID+1); prs != nil {
		t.Fatalf("another item's prs = %+v", prs)
	}
}

// One job of a kind across two processes, as `factory tune` and the
// daemon are: the second EnqueueSole queues nothing.
func TestEnqueueSoleAcrossHandles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, path := openTest(t)
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok, err := s.EnqueueSole(ctx, Job{Kind: "tune", Harness: "claude", Model: "opus", BudgetUSD: 3}); err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	if id, ok, err := s2.EnqueueSole(ctx, Job{Kind: "tune", Harness: "claude", Model: "opus", BudgetUSD: 3}); err != nil || ok || id != 0 {
		t.Fatalf("second = %d %v, %v", id, ok, err)
	}
	if _, ok, _ := s2.EnqueueSole(ctx, Job{Kind: "orchestrate", Harness: "claude", Model: "opus", BudgetUSD: 3}); !ok {
		t.Fatal("another kind was refused")
	}
}
