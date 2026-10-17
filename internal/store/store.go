// Package store is the daemon's SQLite store: one database file in WAL mode,
// forward-only migrations, and a single writer goroutine. Workers never touch
// the database; only the daemon does.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/claptimes5/factory/internal/state"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// TimeFormat is fixed width so that text comparison orders by time.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// Now returns the current time in the store's text format.
func Now() string { return time.Now().UTC().Format(TimeFormat) }

// Today returns the current UTC day, the key of the spend table.
func Today() string { return time.Now().UTC().Format("2006-01-02") }

// Store wraps one SQLite file. Reads use a pool; every write goes through
// one goroutine so the daemon never contends with itself for the lock.
type Store struct {
	path   string
	read   *sql.DB
	write  *sql.DB
	writes chan writeReq
	done   chan struct{}
}

type writeReq struct {
	ctx context.Context
	fn  func(*sql.Tx) error
	res chan error
}

func dsn(path string, immediate bool) string {
	q := "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if immediate {
		q += "&_txlock=immediate"
	}
	return "file:" + path + q
}

// Open opens (creating if needed) the database and applies pending migrations.
func Open(path string) (*Store, error) {
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		w.Close()
		return nil, err
	}
	s := &Store{path: path, read: r, write: w, writes: make(chan writeReq), done: make(chan struct{})}
	if err := s.migrate(); err != nil {
		w.Close()
		r.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	go s.writer()
	return s, nil
}

// Close stops the writer and closes the database.
func (s *Store) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	close(s.done)
	s.write.Close()
	return s.read.Close()
}

func (s *Store) writer() {
	for {
		select {
		case <-s.done:
			return
		case req := <-s.writes:
			req.res <- s.runTx(req.ctx, req.fn)
		}
	}
}

func (s *Store) runTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// tx runs fn in a write transaction on the writer goroutine.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	req := writeReq{ctx: ctx, fn: fn, res: make(chan error, 1)}
	select {
	case s.writes <- req:
	case <-s.done:
		return errors.New("store closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-req.res
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: want NNNN_name.sql", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..n without gaps; got %s at position %d", m.name, i+1)
		}
	}
	return ms, nil
}

// migrate applies pending migrations in order. Before migrating a database
// that already holds a schema, it writes a backup next to it.
func (s *Store) migrate() error {
	ctx := context.Background()
	if _, err := s.write.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if cur > len(ms) {
		return fmt.Errorf("database schema version %d is newer than this binary (%d); refusing to run", cur, len(ms))
	}
	for _, m := range ms[cur:] {
		if cur > 0 {
			bak := fmt.Sprintf("%s.bak-v%d-%s", s.path, cur, time.Now().UTC().Format("20060102T150405"))
			if _, err := s.write.ExecContext(ctx, `VACUUM INTO ?`, bak); err != nil {
				return fmt.Errorf("backup before %s: %w", m.name, err)
			}
		}
		err := s.runTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(m.sql); err != nil {
				return fmt.Errorf("%s: %w", m.name, err)
			}
			_, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, m.version, Now())
			return err
		})
		if err != nil {
			return err
		}
		cur = m.version
	}
	return nil
}

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.write.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Backup writes a consistent copy of the database to dst.
func (s *Store) Backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already exists", dst)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`VACUUM INTO ?`, dst)
		return err
	})
}

// --- work items ---

// WorkItem is one GitHub issue moving through the pipeline.
type WorkItem struct {
	ID          int64
	Repo        string
	IssueNumber int
	Title       string
	Stage       state.Stage
	Complexity  string
	Risk        string
	Priority    int
	ParentID    *int64
	Branch      string
	PRNumber    int
	CreatedAt   string
	UpdatedAt   string
}

const itemCols = `id, repo, issue_number, title, stage, COALESCE(complexity,''), COALESCE(risk,''), COALESCE(priority,0), parent_id, COALESCE(branch,''), COALESCE(pr_number,0), created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanItem(r scanner) (*WorkItem, error) {
	var it WorkItem
	var parent sql.NullInt64
	var stage string
	if err := r.Scan(&it.ID, &it.Repo, &it.IssueNumber, &it.Title, &stage, &it.Complexity, &it.Risk, &it.Priority, &parent, &it.Branch, &it.PRNumber, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return nil, err
	}
	it.Stage = state.Stage(stage)
	if parent.Valid {
		it.ParentID = &parent.Int64
	}
	return &it, nil
}

// UpsertItem returns the work item for an issue, creating it in triage.
func (s *Store) UpsertItem(ctx context.Context, repo string, issue int, title string) (*WorkItem, bool, error) {
	return s.UpsertItemIn(ctx, repo, issue, title, state.Triage)
}

// UpsertItemIn is UpsertItem with the stage a new item starts in.
func (s *Store) UpsertItemIn(ctx context.Context, repo string, issue int, title string, stage state.Stage) (*WorkItem, bool, error) {
	var created bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := Now()
		res, err := tx.Exec(`INSERT INTO work_items(repo, issue_number, title, stage, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(repo, issue_number) DO NOTHING`,
			repo, issue, title, string(stage), now, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		created = n == 1
		if created {
			id, _ := res.LastInsertId()
			return insertEvent(tx, Event{Actor: "daemon", ItemID: &id, Type: "item_created", Reason: fmt.Sprintf("%s#%d", repo, issue)})
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	it, err := s.ItemByIssue(ctx, repo, issue)
	return it, created, err
}

// GetItem loads a work item by id.
func (s *Store) GetItem(ctx context.Context, id int64) (*WorkItem, error) {
	return scanItem(s.read.QueryRowContext(ctx, `SELECT `+itemCols+` FROM work_items WHERE id = ?`, id))
}

// ItemByIssue loads a work item by repo and issue number.
func (s *Store) ItemByIssue(ctx context.Context, repo string, issue int) (*WorkItem, error) {
	return scanItem(s.read.QueryRowContext(ctx, `SELECT `+itemCols+` FROM work_items WHERE repo = ? AND issue_number = ?`, repo, issue))
}

// ListItems returns work items, optionally filtered to some stages.
func (s *Store) ListItems(ctx context.Context, stages ...state.Stage) ([]*WorkItem, error) {
	q := `SELECT ` + itemCols + ` FROM work_items`
	var args []any
	if len(stages) > 0 {
		q += ` WHERE stage IN (` + strings.TrimSuffix(strings.Repeat("?,", len(stages)), ",") + `)`
		for _, st := range stages {
			args = append(args, string(st))
		}
	}
	q += ` ORDER BY priority DESC, id`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WorkItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetStage moves an item to a new stage if the transition is allowed, and
// records why in the audit trail.
func (s *Store) SetStage(ctx context.Context, id int64, to state.Stage, actor, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var from string
		if err := tx.QueryRow(`SELECT stage FROM work_items WHERE id = ?`, id).Scan(&from); err != nil {
			return err
		}
		if state.Stage(from) == to {
			return nil // already there: SetStage is idempotent
		}
		if err := state.CheckStage(state.Stage(from), to); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE work_items SET stage = ?, updated_at = ? WHERE id = ?`, string(to), Now(), id); err != nil {
			return err
		}
		return insertEvent(tx, Event{Actor: actor, ItemID: &id, Type: "stage", Reason: reason, Data: fmt.Sprintf(`{"from":%q,"to":%q}`, from, to)})
	})
}

// ItemUpdate holds optional field changes for UpdateItem.
type ItemUpdate struct {
	Title      *string
	Complexity *string
	Risk       *string
	Branch     *string
	PRNumber   *int
	Priority   *int
}

// UpdateItem changes item fields other than stage.
func (s *Store) UpdateItem(ctx context.Context, id int64, u ItemUpdate) error {
	var sets []string
	var args []any
	add := func(col string, v any) { sets = append(sets, col+" = ?"); args = append(args, v) }
	if u.Title != nil {
		add("title", *u.Title)
	}
	if u.Complexity != nil {
		add("complexity", *u.Complexity)
	}
	if u.Risk != nil {
		add("risk", *u.Risk)
	}
	if u.Branch != nil {
		add("branch", *u.Branch)
	}
	if u.PRNumber != nil {
		add("pr_number", *u.PRNumber)
	}
	if u.Priority != nil {
		add("priority", *u.Priority)
	}
	if len(sets) == 0 {
		return nil
	}
	add("updated_at", Now())
	args = append(args, id)
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE work_items SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		return err
	})
}

// --- jobs ---

// Job is one agent run.
type Job struct {
	ID          int64
	ItemID      *int64
	Kind        string
	State       state.JobState
	Harness     string
	Model       string
	Effort      string
	Attempt     int
	Priority    int
	ContainerID string
	BudgetUSD   float64
	CostUSD     float64
	TokensIn    int64
	TokensOut   int64
	Verdict     string
	Error       string
	LogPath     string
	QueuedAt    string
	StartedAt   string
	FinishedAt  string
	BaseSHA     string
	Branch      string
	Skillset    string
	CompareID   string
	Input       string // JSON stage input, e.g. reviewer findings for a fix round
	HeadSHA     string // commit an implement job produced
}

const jobCols = `id, item_id, kind, state, harness, model, COALESCE(effort,''), attempt, COALESCE(priority,0), COALESCE(container_id,''),
	budget_usd, COALESCE(cost_usd,0), COALESCE(tokens_in,0), COALESCE(tokens_out,0), COALESCE(verdict,''), COALESCE(error,''), COALESCE(log_path,''),
	queued_at, COALESCE(started_at,''), COALESCE(finished_at,''), COALESCE(base_sha,''), COALESCE(branch,''), COALESCE(skillset,''), COALESCE(compare_id,''), COALESCE(input,''), COALESCE(head_sha,'')`

func scanJob(r scanner) (*Job, error) {
	var j Job
	var item sql.NullInt64
	var st string
	if err := r.Scan(&j.ID, &item, &j.Kind, &st, &j.Harness, &j.Model, &j.Effort, &j.Attempt, &j.Priority, &j.ContainerID,
		&j.BudgetUSD, &j.CostUSD, &j.TokensIn, &j.TokensOut, &j.Verdict, &j.Error, &j.LogPath,
		&j.QueuedAt, &j.StartedAt, &j.FinishedAt, &j.BaseSHA, &j.Branch, &j.Skillset, &j.CompareID, &j.Input, &j.HeadSHA); err != nil {
		return nil, err
	}
	j.State = state.JobState(st)
	if item.Valid {
		j.ItemID = &item.Int64
	}
	return &j, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EnqueueJob inserts a queued job and returns its id.
func (s *Store) EnqueueJob(ctx context.Context, j Job) (int64, error) {
	if j.Attempt == 0 {
		j.Attempt = 1
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO jobs(item_id, kind, state, harness, model, effort, attempt, priority, budget_usd, queued_at, base_sha, branch, skillset, compare_id, input)
			VALUES (?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.ItemID, j.Kind, j.Harness, j.Model, nullable(j.Effort), j.Attempt, j.Priority, j.BudgetUSD, Now(),
			nullable(j.BaseSHA), nullable(j.Branch), nullable(j.Skillset), nullable(j.CompareID), nullable(j.Input))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return insertEvent(tx, Event{Actor: "daemon", ItemID: j.ItemID, JobID: &id, Type: "job_queued",
			Data: fmt.Sprintf(`{"kind":%q,"harness":%q,"model":%q,"attempt":%d,"skillset":%q}`, j.Kind, j.Harness, j.Model, j.Attempt, j.Skillset)})
	})
	return id, err
}

// ClaimJob atomically moves the highest-priority, oldest queued job to
// running and returns it, or returns nil when the queue is empty.
func (s *Store) ClaimJob(ctx context.Context) (*Job, error) {
	return s.claim(ctx, "")
}

// ClaimJobExcept is ClaimJob restricted to kinds not in the excluded list,
// used when a cap (such as awaiting-human) pauses one kind of work.
func (s *Store) ClaimJobExcept(ctx context.Context, kinds ...string) (*Job, error) {
	if len(kinds) == 0 {
		return s.claim(ctx, "")
	}
	return s.claim(ctx, ` AND kind NOT IN (`+strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")+`)`, toAny(kinds)...)
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (s *Store) claim(ctx context.Context, filter string, args ...any) (*Job, error) {
	var j *Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		all := append([]any{Now()}, args...)
		row := tx.QueryRow(`UPDATE jobs SET state = 'running', started_at = ?
			WHERE id = (
			  SELECT id FROM jobs WHERE state = 'queued'`+filter+`
			  ORDER BY priority DESC, queued_at, id LIMIT 1
			)
			RETURNING `+jobCols, all...)
		var err error
		j, err = scanJob(row)
		if errors.Is(err, sql.ErrNoRows) {
			j = nil
			return nil
		}
		if err != nil {
			return err
		}
		return insertEvent(tx, Event{Actor: "daemon", ItemID: j.ItemID, JobID: &j.ID, Type: "job_claimed"})
	})
	return j, err
}

// GetJob loads a job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.read.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

// JobFilter selects jobs for ListJobs. Zero fields match everything.
type JobFilter struct {
	ItemID    int64
	Kind      string
	State     state.JobState
	CompareID string
	Limit     int
}

// ListJobs returns jobs matching f, newest first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]*Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs WHERE 1=1`
	var args []any
	if f.ItemID != 0 {
		q += ` AND item_id = ?`
		args = append(args, f.ItemID)
	}
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, string(f.State))
	}
	if f.CompareID != "" {
		q += ` AND compare_id = ?`
		args = append(args, f.CompareID)
	}
	q += ` ORDER BY id DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CountJobs counts jobs in a state, optionally of one kind.
func (s *Store) CountJobs(ctx context.Context, st state.JobState, kind string) (int, error) {
	q := `SELECT COUNT(*) FROM jobs WHERE state = ?`
	args := []any{string(st)}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	var n int
	err := s.read.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// SetJobRuntime records where a running job's container and log live.
func (s *Store) SetJobRuntime(ctx context.Context, id int64, containerID, logPath, baseSHA string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs SET container_id = COALESCE(?, container_id), log_path = COALESCE(?, log_path), base_sha = COALESCE(?, base_sha) WHERE id = ?`,
			nullable(containerID), nullable(logPath), nullable(baseSHA), id)
		return err
	})
}

// SetJobHead records the commit an implement job produced.
func (s *Store) SetJobHead(ctx context.Context, id int64, sha string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs SET head_sha = ? WHERE id = ?`, sha, id)
		return err
	})
}

// Result is a job's outcome as recorded by FinishJob.
type Result struct {
	State     state.JobState
	CostUSD   float64
	TokensIn  int64
	TokensOut int64
	Verdict   string
	Error     string
}

// FinishJob moves a running job to a terminal state, records its usage and
// adds its cost to today's spend.
func (s *Store) FinishJob(ctx context.Context, id int64, r Result) error {
	if !r.State.Terminal() {
		return fmt.Errorf("FinishJob needs a terminal state, got %s", r.State)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var from string
		var item sql.NullInt64
		if err := tx.QueryRow(`SELECT state, item_id FROM jobs WHERE id = ?`, id).Scan(&from, &item); err != nil {
			return err
		}
		if err := state.CheckJob(state.JobState(from), r.State); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE jobs SET state = ?, cost_usd = ?, tokens_in = ?, tokens_out = ?, verdict = ?, error = ?, finished_at = ? WHERE id = ?`,
			string(r.State), r.CostUSD, r.TokensIn, r.TokensOut, nullable(r.Verdict), nullable(r.Error), Now(), id); err != nil {
			return err
		}
		if r.CostUSD > 0 {
			if _, err := tx.Exec(`INSERT INTO spend(day, cost_usd) VALUES (?, ?) ON CONFLICT(day) DO UPDATE SET cost_usd = cost_usd + excluded.cost_usd`, Today(), r.CostUSD); err != nil {
				return err
			}
		}
		var itemID *int64
		if item.Valid {
			itemID = &item.Int64
		}
		return insertEvent(tx, Event{Actor: "daemon", ItemID: itemID, JobID: &id, Type: "job_finished", Reason: r.Error,
			Data: fmt.Sprintf(`{"state":%q,"cost_usd":%g}`, r.State, r.CostUSD)})
	})
}

// RequeueRunning is the restart reconciliation: every running job whose
// container is not alive goes back to queued without using a retry.
func (s *Store) RequeueRunning(ctx context.Context, alive func(*Job) bool) (int, error) {
	running, err := s.ListJobs(ctx, JobFilter{State: state.Running})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range running {
		if alive(j) {
			continue
		}
		err := s.tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.Exec(`UPDATE jobs SET state = 'queued', started_at = NULL, container_id = NULL WHERE id = ? AND state = 'running'`, j.ID)
			if err != nil {
				return err
			}
			if c, _ := res.RowsAffected(); c == 1 {
				n++
				return insertEvent(tx, Event{Actor: "daemon", ItemID: j.ItemID, JobID: &j.ID, Type: "job_requeued", Reason: "daemon restart: no live container"})
			}
			return nil
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// --- gates, spend, events ---

// GateResult is one deterministic gate's outcome for a job.
type GateResult struct {
	Gate   string
	Status string // pass|fail|misconfigured
	Detail string
}

// SetGateResults replaces a job's gate results.
func (s *Store) SetGateResults(ctx context.Context, jobID int64, rs []GateResult) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM gate_results WHERE job_id = ?`, jobID); err != nil {
			return err
		}
		for _, r := range rs {
			if _, err := tx.Exec(`INSERT INTO gate_results(job_id, gate, status, detail) VALUES (?, ?, ?, ?)`, jobID, r.Gate, r.Status, r.Detail); err != nil {
				return err
			}
		}
		return nil
	})
}

// GateResults returns a job's gate results.
func (s *Store) GateResults(ctx context.Context, jobID int64) ([]GateResult, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT gate, status, COALESCE(detail,'') FROM gate_results WHERE job_id = ? ORDER BY gate`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateResult
	for rows.Next() {
		var r GateResult
		if err := rows.Scan(&r.Gate, &r.Status, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddSpend adds cost spent outside this store's jobs (a compare run's
// arms) to today's spend, with an event saying what it was.
func (s *Store) AddSpend(ctx context.Context, cost float64, actor, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO spend(day, cost_usd) VALUES (?, ?) ON CONFLICT(day) DO UPDATE SET cost_usd = cost_usd + excluded.cost_usd`, Today(), cost); err != nil {
			return err
		}
		return insertEvent(tx, Event{Actor: actor, Type: "spend", Reason: reason, Data: fmt.Sprintf(`{"cost_usd":%g}`, cost)})
	})
}

// SpendOn returns the recorded spend for a day (YYYY-MM-DD).
func (s *Store) SpendOn(ctx context.Context, day string) (float64, error) {
	var v sql.NullFloat64
	err := s.read.QueryRowContext(ctx, `SELECT cost_usd FROM spend WHERE day = ?`, day).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v.Float64, err
}

// Event is one row of the append-only audit trail.
type Event struct {
	ID     int64
	At     string
	Actor  string
	ItemID *int64
	JobID  *int64
	Type   string
	Reason string
	Data   string
}

func insertEvent(tx *sql.Tx, e Event) error {
	if e.At == "" {
		e.At = Now()
	}
	_, err := tx.Exec(`INSERT INTO events(at, actor, item_id, job_id, type, reason, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.At, e.Actor, e.ItemID, e.JobID, e.Type, nullable(e.Reason), nullable(e.Data))
	return err
}

// AddEvent appends to the audit trail.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return insertEvent(tx, e) })
}

// Events returns an item's audit trail, oldest first (all events if itemID is 0).
func (s *Store) Events(ctx context.Context, itemID int64) ([]Event, error) {
	q := `SELECT id, at, actor, item_id, job_id, type, COALESCE(reason,''), COALESCE(data,'') FROM events`
	var args []any
	if itemID != 0 {
		q += ` WHERE item_id = ?`
		args = append(args, itemID)
	}
	q += ` ORDER BY id`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var item, job sql.NullInt64
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &item, &job, &e.Type, &e.Reason, &e.Data); err != nil {
			return nil, err
		}
		if item.Valid {
			e.ItemID = &item.Int64
		}
		if job.Valid {
			e.JobID = &job.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CancelQueued cancels a job that has not started.
func (s *Store) CancelQueued(ctx context.Context, id int64, actor, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var item sql.NullInt64
		res, err := tx.Exec(`UPDATE jobs SET state = 'cancelled', finished_at = ?, error = ? WHERE id = ? AND state = 'queued'`, Now(), reason, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("job %d is not queued", id)
		}
		if err := tx.QueryRow(`SELECT item_id FROM jobs WHERE id = ?`, id).Scan(&item); err != nil {
			return err
		}
		var itemID *int64
		if item.Valid {
			itemID = &item.Int64
		}
		return insertEvent(tx, Event{Actor: actor, ItemID: itemID, JobID: &id, Type: "job_cancelled", Reason: reason})
	})
}

// Dependencies returns the ids of items an item depends on.
func (s *Store) Dependencies(ctx context.Context, itemID int64) ([]int64, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT depends_on_id FROM dependencies WHERE item_id = ? ORDER BY depends_on_id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EventsByActor returns the newest events by one actor, newest first.
func (s *Store) EventsByActor(ctx context.Context, actor string, limit int) ([]Event, error) {
	return s.EventsByActorFor(ctx, actor, 0, limit)
}

// EventsByActorFor is EventsByActor limited to one item (all items if 0).
func (s *Store) EventsByActorFor(ctx context.Context, actor string, itemID int64, limit int) ([]Event, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, at, actor, item_id, job_id, type, COALESCE(reason,''), COALESCE(data,'')
		FROM events WHERE actor = ? AND (? = 0 OR item_id = ?) ORDER BY id DESC LIMIT ?`, actor, itemID, itemID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var item, job sql.NullInt64
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &item, &job, &e.Type, &e.Reason, &e.Data); err != nil {
			return nil, err
		}
		if item.Valid {
			e.ItemID = &item.Int64
		}
		if job.Valid {
			e.JobID = &job.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SpendByKind sums the cost of jobs of one kind that finished on a day.
func (s *Store) SpendByKind(ctx context.Context, day, kind string) (float64, error) {
	var v sql.NullFloat64
	err := s.read.QueryRowContext(ctx, `SELECT SUM(cost_usd) FROM jobs WHERE kind = ? AND substr(finished_at, 1, 10) = ?`, kind, day).Scan(&v)
	return v.Float64, err
}

// CountEventsSince counts events of a type by an actor at or after a time.
func (s *Store) CountEventsSince(ctx context.Context, actor, typ, since string) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE actor = ? AND type = ? AND at >= ?`, actor, typ, since).Scan(&n)
	return n, err
}

// HasEvent reports whether an event with this type and reason exists.
func (s *Store) HasEvent(ctx context.Context, typ, reason string) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type = ? AND reason = ?`, typ, reason).Scan(&n)
	return n > 0, err
}

// ErrCycle is returned when a dependency would create a cycle.
var ErrCycle = errors.New("dependency cycle")

// SetDependencies replaces an item's dependencies, refusing cycles.
func (s *Store) SetDependencies(ctx context.Context, itemID int64, dependsOn []int64, actor, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT item_id, depends_on_id FROM dependencies WHERE item_id != ?`, itemID)
		if err != nil {
			return err
		}
		graph := map[int64][]int64{}
		for rows.Next() {
			var a, b int64
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return err
			}
			graph[a] = append(graph[a], b)
		}
		rows.Close()
		graph[itemID] = dependsOn
		// A cycle through itemID exists if itemID is reachable from its deps.
		seen := map[int64]bool{}
		var reach func(n int64) bool
		reach = func(n int64) bool {
			if n == itemID {
				return true
			}
			if seen[n] {
				return false
			}
			seen[n] = true
			for _, m := range graph[n] {
				if reach(m) {
					return true
				}
			}
			return false
		}
		for _, d := range dependsOn {
			if reach(d) {
				return fmt.Errorf("%w: %d -> %d leads back to %d", ErrCycle, itemID, d, itemID)
			}
		}
		if _, err := tx.Exec(`DELETE FROM dependencies WHERE item_id = ?`, itemID); err != nil {
			return err
		}
		for _, d := range dependsOn {
			if _, err := tx.Exec(`INSERT INTO dependencies(item_id, depends_on_id) VALUES (?, ?)`, itemID, d); err != nil {
				return err
			}
		}
		b, _ := json.Marshal(dependsOn)
		return insertEvent(tx, Event{Actor: actor, ItemID: &itemID, Type: "set_dependencies", Reason: reason, Data: string(b)})
	})
}

// SetPriority changes an item's priority and that of its queued jobs.
func (s *Store) SetPriority(ctx context.Context, itemID int64, prio int) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE work_items SET priority = ?, updated_at = ? WHERE id = ?`, prio, Now(), itemID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE jobs SET priority = ? WHERE item_id = ? AND state = 'queued'`, prio, itemID)
		return err
	})
}

// SetParent records an epic's child.
func (s *Store) SetParent(ctx context.Context, itemID, parentID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE work_items SET parent_id = ?, updated_at = ? WHERE id = ?`, parentID, Now(), itemID)
		return err
	})
}

// SetJobInput replaces a queued job's input (used to coalesce triggers).
// SetJobInput replaces a queued job's input. It reports false when the job
// was no longer queued.
func (s *Store) SetJobInput(ctx context.Context, id int64, input string) (bool, error) {
	var ok bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE jobs SET input = ? WHERE id = ? AND state = 'queued'`, input, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n == 1
		return err
	})
	return ok, err
}

// Version is a cheap fingerprint of everything the dashboard shows: it
// changes whenever an event is written or a job changes state, cost or
// head. Clients poll it (over SSE) and refetch when it moves.
func (s *Store) Version(ctx context.Context) (string, error) {
	var ev, jobs, finished int64
	var cost float64
	var live, items string
	err := s.read.QueryRowContext(ctx, `SELECT
		(SELECT COALESCE(MAX(id),0) FROM events),
		(SELECT COALESCE(MAX(id),0) FROM jobs),
		(SELECT COUNT(*) FROM jobs WHERE finished_at IS NOT NULL),
		(SELECT COALESCE(SUM(cost_usd),0) FROM jobs),
		(SELECT COALESCE(GROUP_CONCAT(id || state || COALESCE(head_sha,''), ','),'') FROM jobs WHERE state IN ('queued','running')),
		(SELECT COUNT(*) || '/' || COALESCE(MAX(updated_at),'') FROM work_items)`).
		Scan(&ev, &jobs, &finished, &cost, &live, &items)
	if err != nil {
		return "", err
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%d|%.6f|%s|%s", ev, jobs, finished, cost, live, items)
	return strconv.FormatUint(h.Sum64(), 36), nil
}
