package persist

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"exectop/src/internal/fault"
	"exectop/src/metrics"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"fmt"
	"io"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Item struct{ Result runner.Result }

func UpsertCheck(ctx context.Context, db *sql.DB, s spec.Spec, grp string) error {
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	now := time.Now().UnixNano()
	_, e = db.ExecContext(ctx, `INSERT INTO checks(check_id,name,grp,path,spec_json,spec_hash,first_seen,last_seen,deleted_at) VALUES(?,?,?,?,?,?,?, ?,NULL) ON CONFLICT(check_id) DO UPDATE SET name=excluded.name,grp=excluded.grp,path=excluded.path,spec_json=excluded.spec_json,spec_hash=excluded.spec_hash,last_seen=excluded.last_seen,deleted_at=NULL`, s.ID, s.Name, grp, s.Path, b, s.Hash, now, now)
	return e
}
func MarkDeleted(ctx context.Context, db *sql.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if _, e := db.ExecContext(ctx, `UPDATE checks SET deleted_at=? WHERE check_id=?`, time.Now().UnixNano(), id); e != nil {
			return e
		}
	}
	return nil
}

func RecordEvent(ctx context.Context, db *sql.DB, kind, detail string) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `INSERT INTO daemon_events(at,kind,detail) VALUES(?,?,?)`, time.Now().UnixNano(), kind, detail)
	return err
}

type WriterStats struct{ Depth, QueueDepthMax, Dropped, LastCommitMS int64 }
type WriterConfig struct {
	QueueSize     int
	BatchSize     int
	BatchInterval time.Duration
	CompressOver  int
	Block         bool
	CommitDelay   time.Duration
}
type Writer struct {
	db            *sql.DB
	q             chan Item
	done          chan struct{}
	wg            sync.WaitGroup
	mu            sync.Mutex
	dropped       int64
	closed        bool
	closeCtx      context.Context
	cfg           WriterConfig
	lastCommitMS  int64
	queueDepthMax int64
}

const schema = `CREATE TABLE IF NOT EXISTS schema_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);CREATE TABLE IF NOT EXISTS checks(check_id TEXT PRIMARY KEY,name TEXT NOT NULL,grp TEXT NOT NULL DEFAULT '',path TEXT NOT NULL,spec_json TEXT NOT NULL,spec_hash TEXT NOT NULL,first_seen INTEGER NOT NULL,last_seen INTEGER NOT NULL,deleted_at INTEGER);CREATE TABLE IF NOT EXISTS runs(run_id INTEGER PRIMARY KEY,check_id TEXT NOT NULL REFERENCES checks(check_id),iter INTEGER NOT NULL,trigger TEXT NOT NULL,status TEXT NOT NULL,exit_code INTEGER,signal TEXT,reason TEXT,started_at INTEGER NOT NULL,ended_at INTEGER NOT NULL,duration_ms INTEGER NOT NULL,summary TEXT NOT NULL DEFAULT '',truncated INTEGER NOT NULL DEFAULT 0,spec_hash TEXT NOT NULL);CREATE INDEX IF NOT EXISTS runs_check_started ON runs(check_id,started_at DESC);CREATE INDEX IF NOT EXISTS runs_started ON runs(started_at);CREATE INDEX IF NOT EXISTS runs_status ON runs(status,started_at DESC);CREATE TABLE IF NOT EXISTS run_output(run_id INTEGER PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,stdout BLOB,stderr BLOB,encoding TEXT NOT NULL DEFAULT 'raw');CREATE TABLE IF NOT EXISTS run_metrics(run_id INTEGER NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,key TEXT NOT NULL,value REAL NOT NULL,PRIMARY KEY(run_id,key));CREATE TABLE IF NOT EXISTS latest(check_id TEXT PRIMARY KEY REFERENCES checks(check_id),run_id INTEGER NOT NULL REFERENCES runs(run_id),status TEXT NOT NULL,started_at INTEGER NOT NULL,ended_at INTEGER NOT NULL,summary TEXT NOT NULL DEFAULT '',streak INTEGER NOT NULL DEFAULT 1,streak_since INTEGER NOT NULL,iter INTEGER NOT NULL,lease_until INTEGER);CREATE TABLE IF NOT EXISTS leases(check_id TEXT PRIMARY KEY,lease_until INTEGER NOT NULL);CREATE TABLE IF NOT EXISTS transitions(id INTEGER PRIMARY KEY,check_id TEXT NOT NULL REFERENCES checks(check_id),run_id INTEGER NOT NULL REFERENCES runs(run_id),from_status TEXT,to_status TEXT NOT NULL,at INTEGER NOT NULL);CREATE INDEX IF NOT EXISTS transitions_check_at ON transitions(check_id,at DESC);CREATE TABLE IF NOT EXISTS daemon_events(id INTEGER PRIMARY KEY,at INTEGER NOT NULL,kind TEXT NOT NULL,detail TEXT);`

func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_ = os.Chmod(path, 0600)
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA wal_autocheckpoint=4096", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", "PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA temp_store=MEMORY"} {
		if _, e = db.Exec(p); e != nil {
			db.Close()
			if strings.Contains(strings.ToLower(e.Error()), "not a database") {
				return recoverCorrupt(path)
			}
			return nil, e
		}
	}
	var check string
	if e = db.QueryRow("PRAGMA quick_check").Scan(&check); e != nil || check != "ok" {
		_ = db.Close()
		return recoverCorrupt(path)
	}
	if _, e = db.Exec(schema); e != nil {
		db.Close()
		return nil, e
	}
	var version string
	if e = db.QueryRow(`SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&version); e == nil && version != "1" {
		_ = db.Close()
		return nil, fmt.Errorf("unsupported schema version: %s", version)
	}
	_, _ = db.Exec(`INSERT INTO schema_meta(key,value) VALUES('schema_version','1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	return db, nil
}

func recoverCorrupt(path string) (*sql.DB, error) {
	backup := path + ".corrupt-" + time.Now().Format("20060102150405.000000000")
	if e := os.Rename(path, backup); e != nil {
		return nil, e
	}
	db, e := Open(path)
	if e == nil {
		if eventErr := RecordEvent(context.Background(), db, "corrupt_recovered", "database moved to "+backup); eventErr != nil {
			_ = db.Close()
			return nil, eventErr
		}
	}
	return db, e
}
func NewWriter(db *sql.DB, size int) *Writer {
	return NewWriterWithConfig(db, WriterConfig{QueueSize: size})
}
func NewWriterWithConfig(db *sql.DB, cfg WriterConfig) *Writer {
	if cfg.QueueSize < 1 {
		cfg.QueueSize = 4096
	}
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 200
	}
	if cfg.BatchInterval <= 0 {
		cfg.BatchInterval = 200 * time.Millisecond
	}
	if cfg.CompressOver <= 0 {
		cfg.CompressOver = 4096
	}
	w := &Writer{db: db, q: make(chan Item, cfg.QueueSize), done: make(chan struct{}), cfg: cfg}
	w.wg.Add(1)
	go w.loop()
	return w
}
func (w *Writer) Enqueue(i Item) bool {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return false
	}
	if w.cfg.Block {
		select {
		case w.q <- i:
		case <-w.done:
			return false
		}
		w.mu.Lock()
		if n := int64(len(w.q)); n > w.queueDepthMax {
			w.queueDepthMax = n
		}
		w.mu.Unlock()
		return true
	}
	select {
	case w.q <- i:
		w.mu.Lock()
		if n := int64(len(w.q)); n > w.queueDepthMax {
			w.queueDepthMax = n
		}
		w.mu.Unlock()
		return true
	default:
		w.mu.Lock()
		w.dropped++
		w.mu.Unlock()
		return false
	}
}
func (w *Writer) Stats() WriterStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WriterStats{Depth: int64(len(w.q)), QueueDepthMax: w.queueDepthMax, Dropped: w.dropped, LastCommitMS: w.lastCommitMS}
}
func (w *Writer) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		w.closeCtx = ctx
		close(w.done)
	}
	w.mu.Unlock()
	ch := make(chan struct{})
	go func() { w.wg.Wait(); close(ch) }()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *Writer) loop() {
	defer w.wg.Done()
	t := time.NewTicker(w.cfg.BatchInterval)
	defer t.Stop()
	batch := make([]Item, 0, w.cfg.BatchSize)
	flush := func(ctx context.Context) bool {
		if len(batch) == 0 {
			return true
		}
		start := time.Now()
		for attempt := 0; attempt < 3; attempt++ {
			if w.writeBatch(ctx, batch) == nil {
				break
			}
			if attempt == 2 {
				w.mu.Lock()
				w.dropped += int64(len(batch))
				w.mu.Unlock()
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				w.mu.Lock()
				w.dropped += int64(len(batch))
				w.mu.Unlock()
				batch = batch[:0]
				return false
			}
		}
		w.mu.Lock()
		w.lastCommitMS = time.Since(start).Milliseconds()
		w.mu.Unlock()
		batch = batch[:0]
		return true
	}
	for {
		select {
		case i := <-w.q:
			batch = append(batch, i)
			if len(batch) >= w.cfg.BatchSize {
				_ = flush(context.Background())
			}
		case <-t.C:
			_ = flush(context.Background())
		case <-w.done:
			w.mu.Lock()
			ctx := w.closeCtx
			w.mu.Unlock()
			if ctx == nil {
				ctx = context.Background()
			}
			for {
				select {
				case i := <-w.q:
					batch = append(batch, i)
					if len(batch) >= w.cfg.BatchSize {
						if !flush(ctx) {
							return
						}
					}
				case <-ctx.Done():
					w.mu.Lock()
					w.dropped += int64(len(batch) + len(w.q))
					w.mu.Unlock()
					return
				default:
					_ = flush(ctx)
					return
				}
			}
		}
	}
}
func (w *Writer) writeBatch(ctx context.Context, items []Item) error {
	started := time.Now()
	defer func() { metrics.Observe("persist.commit", time.Since(started)) }()
	fault.Hang("persist.commit_hang")
	tx, e := w.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	for _, i := range items {
		if e = writeTx(tx, i.Result, w.cfg.CompressOver); e != nil {
			_ = tx.Rollback()
			return e
		}
	}
	if w.cfg.CommitDelay > 0 {
		t := time.NewTimer(w.cfg.CommitDelay)
		select {
		case <-t.C:
		case <-ctx.Done():
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			_ = tx.Rollback()
			return ctx.Err()
		}
	}
	return tx.Commit()
}
func writeTx(tx *sql.Tx, r runner.Result, compressOver int) error {
	// Results may be written by an embedded writer before the discovery ledger
	// has been populated. Keep the FK contract intact without overwriting a
	// resolved check record.
	if _, e := tx.Exec(`INSERT OR IGNORE INTO checks(check_id,name,grp,path,spec_json,spec_hash,first_seen,last_seen) VALUES(?,?,?,?,?,?,?,?)`, r.ID, r.ID, "", "", `{}`, r.SpecHash, r.Start.UnixNano(), r.Start.UnixNano()); e != nil {
		return e
	}
	var prev sql.NullString
	var streak, streakSince int64
	_ = tx.QueryRow(`SELECT status,streak,streak_since FROM latest WHERE check_id=?`, r.ID).Scan(&prev, &streak, &streakSince)
	if !prev.Valid || prev.String != string(r.Status) {
		streak = 1
		streakSince = r.Start.UnixNano()
	} else {
		streak++
		if streakSince == 0 {
			streakSince = r.Start.UnixNano()
		}
	}
	var exitCode any
	if r.ExitCode >= 0 {
		exitCode = r.ExitCode
	}
	res, e := tx.Exec(`INSERT INTO runs(check_id,iter,trigger,status,exit_code,signal,reason,started_at,ended_at,duration_ms,summary,truncated,spec_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.Iter, r.Trigger, r.Status, exitCode, r.Signal, r.Reason, r.Start.UnixNano(), r.End.UnixNano(), r.Duration.Milliseconds(), r.Summary, boolInt(r.Truncated), r.SpecHash)
	if e != nil {
		return e
	}
	id, _ := res.LastInsertId()
	data1, data2, enc := r.Stdout, r.Stderr, "raw"
	if len(data1)+len(data2) > compressOver {
		data1 = compress(data1)
		data2 = compress(data2)
		enc = "gzip"
	}
	_, e = tx.Exec(`INSERT INTO run_output(run_id,stdout,stderr,encoding) VALUES(?,?,?,?)`, id, data1, data2, enc)
	if e == nil {
		for k, v := range r.Metrics {
			_, e = tx.Exec(`INSERT INTO run_metrics(run_id,key,value) VALUES(?,?,?)`, id, k, v)
			if e != nil {
				return e
			}
		}
	}
	if e == nil {
		if !prev.Valid || prev.String != string(r.Status) {
			_, e = tx.Exec(`INSERT INTO transitions(check_id,run_id,from_status,to_status,at) VALUES(?,?,?,?,?)`, r.ID, id, nullable(prev), r.Status, r.Start.UnixNano())
		}
	}
	if e == nil {
		_, e = tx.Exec(`INSERT INTO latest(check_id,run_id,status,started_at,ended_at,summary,streak,streak_since,iter) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(check_id) DO UPDATE SET run_id=excluded.run_id,status=excluded.status,started_at=excluded.started_at,ended_at=excluded.ended_at,summary=excluded.summary,streak=excluded.streak,streak_since=excluded.streak_since,iter=excluded.iter`, r.ID, id, r.Status, r.Start.UnixNano(), r.End.UnixNano(), r.Summary, streak, streakSince, r.Iter)
	}
	return e
}
func nullable(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

type Latest struct {
	Result   runner.Result
	Iter     int
	Restored bool
}

func LoadLatest(ctx context.Context, db *sql.DB, ids []string) (map[string]Latest, error) {
	out := map[string]Latest{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, e := db.QueryContext(ctx, `SELECT l.check_id,l.status,l.started_at,l.ended_at,l.summary,l.iter,r.exit_code,r.signal,r.reason,r.trigger,r.spec_hash,r.truncated,COALESCE(o.stdout,''),COALESCE(o.stderr,''),COALESCE(o.encoding,'raw'),r.run_id FROM latest l JOIN runs r ON r.run_id=l.run_id LEFT JOIN run_output o ON o.run_id=l.run_id`)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	for _, id := range ids {
		allowed[id] = true
	}
	type latestBase struct {
		id, status, signal, reason, trigger, hash, encoding string
		started, ended                                      int64
		summary                                             string
		iter, truncated, runID                              int
		exit                                                sql.NullInt64
		stdout, stderr                                      []byte
	}
	bases := make([]latestBase, 0)
	for rows.Next() {
		var b latestBase
		if e = rows.Scan(&b.id, &b.status, &b.started, &b.ended, &b.summary, &b.iter, &b.exit, &b.signal, &b.reason, &b.trigger, &b.hash, &b.truncated, &b.stdout, &b.stderr, &b.encoding, &b.runID); e != nil {
			return nil, e
		}
		if !allowed[b.id] {
			continue
		}
		bases = append(bases, b)
	}
	if e = rows.Close(); e != nil {
		return nil, e
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	for _, b := range bases {
		metricsMap := map[string]float64{}
		metricRows, me := db.QueryContext(ctx, `SELECT key,value FROM run_metrics WHERE run_id=?`, b.runID)
		if me == nil {
			for metricRows.Next() {
				var key string
				var value float64
				if me = metricRows.Scan(&key, &value); me != nil {
					break
				}
				metricsMap[key] = value
			}
			_ = metricRows.Close()
		}
		if me != nil {
			return nil, me
		}
		exitCode := -1
		if b.exit.Valid {
			exitCode = int(b.exit.Int64)
		}
		r := runner.Result{ID: b.id, Status: status.Status(b.status), Start: time.Unix(0, b.started), End: time.Unix(0, b.ended), Duration: time.Duration(b.ended - b.started), Summary: b.summary, Stdout: DecodeOutput(b.encoding, b.stdout), Stderr: DecodeOutput(b.encoding, b.stderr), Truncated: b.truncated != 0, Metrics: metricsMap, Iter: b.iter, ExitCode: exitCode, Signal: b.signal, Reason: b.reason, Trigger: runner.Trigger(b.trigger), SpecHash: b.hash}
		r.ParseVariables()
		out[b.id] = Latest{Result: r, Iter: b.iter, Restored: true}
	}
	return out, nil
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func compress(b []byte) []byte {
	var x []byte
	buf := newByteWriter(&x)
	z := gzip.NewWriter(buf)
	z.Write(b)
	z.Close()
	return x
}

type byteWriter struct{ p *[]byte }

func newByteWriter(p *[]byte) *byteWriter         { return &byteWriter{p} }
func (w *byteWriter) Write(b []byte) (int, error) { *w.p = append(*w.p, b...); return len(b), nil }

type RunRow struct {
	ID       int64
	CheckID  string
	Iter     int64
	Status   string
	ExitCode sql.NullInt64
	Started  time.Time
	Duration time.Duration
	Summary  string
}
type TransitionRow struct {
	CheckID, FromStatus, ToStatus string
	At                            time.Time
}

func Transitions(ctx context.Context, db *sql.DB, checkID string, since, until time.Time) ([]TransitionRow, error) {
	if until.IsZero() {
		until = time.Now()
	}
	q := `SELECT check_id,COALESCE(from_status,''),to_status,at FROM transitions WHERE at BETWEEN ? AND ?`
	args := []any{since.UnixNano(), until.UnixNano()}
	if checkID != "" {
		q += ` AND check_id=?`
		args = append(args, checkID)
	}
	q += ` ORDER BY at`
	rows, e := db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []TransitionRow
	for rows.Next() {
		var r TransitionRow
		var ts int64
		if e = rows.Scan(&r.CheckID, &r.FromStatus, &r.ToStatus, &ts); e != nil {
			return nil, e
		}
		r.At = time.Unix(0, ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

func History(ctx context.Context, db *sql.DB, id string, n int) ([]RunRow, error) {
	return HistorySince(ctx, db, id, n, time.Time{})
}
func HistorySince(ctx context.Context, db *sql.DB, id string, n int, since time.Time) ([]RunRow, error) {
	return HistoryBetween(ctx, db, id, n, since, time.Time{})
}
func HistoryBetween(ctx context.Context, db *sql.DB, id string, n int, since, until time.Time) ([]RunRow, error) {
	if n < 1 {
		n = 60
	}
	q := `SELECT run_id,iter,status,exit_code,started_at,duration_ms,summary FROM runs WHERE check_id=?`
	args := []any{id}
	if !since.IsZero() {
		q += ` AND started_at>=?`
		args = append(args, since.UnixNano())
	}
	if !until.IsZero() {
		q += ` AND started_at<=?`
		args = append(args, until.UnixNano())
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, n)
	rows, e := db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		var r RunRow
		var ts, ms int64
		if e = rows.Scan(&r.ID, &r.Iter, &r.Status, &r.ExitCode, &ts, &ms, &r.Summary); e != nil {
			return nil, e
		}
		r.CheckID = id
		r.Started = time.Unix(0, ts)
		r.Duration = time.Duration(ms) * time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}
func DecodeOutput(enc string, b []byte) []byte {
	if enc != "gzip" {
		return b
	}
	z, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return nil
	}
	x, _ := io.ReadAll(z)
	z.Close()
	return x
}

func Vacuum(ctx context.Context, db *sql.DB) error {
	if _, e := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
		return e
	}
	_, e := db.ExecContext(ctx, "PRAGMA incremental_vacuum(256)")
	return e
}
func Prune(ctx context.Context, db *sql.DB, keep int) error {
	return PruneWithOptions(ctx, db, Retention{RunsMax: keep})
}

type Retention struct {
	RunsMax        int
	RunsAge        time.Duration
	OutputAge      time.Duration
	OutputMin      int
	TransitionsAge time.Duration
	EventsAge      time.Duration
}

func PruneWithOptions(ctx context.Context, db *sql.DB, r Retention) error {
	if r.RunsMax < 1 {
		r.RunsMax = 1000
	}
	idsRows, e := db.QueryContext(ctx, `SELECT DISTINCT check_id FROM runs`)
	if e != nil {
		return e
	}
	var ids []string
	for idsRows.Next() {
		var id string
		if e = idsRows.Scan(&id); e != nil {
			idsRows.Close()
			return e
		}
		ids = append(ids, id)
	}
	idsRows.Close()
	cut := time.Time{}
	if r.RunsAge > 0 {
		cut = time.Now().Add(-r.RunsAge)
	}
	for _, id := range ids {
		var latestID int64
		_ = db.QueryRowContext(ctx, `SELECT run_id FROM latest WHERE check_id=?`, id).Scan(&latestID)
		q := `SELECT run_id,started_at FROM runs WHERE check_id=? ORDER BY started_at DESC,run_id DESC`
		args := []any{id}
		rows, e := db.QueryContext(ctx, q, args...)
		if e != nil {
			return e
		}
		var runIDs []int64
		position := 0
		for rows.Next() {
			var n int64
			var started int64
			if e = rows.Scan(&n, &started); e != nil {
				rows.Close()
				return e
			}
			if n != latestID && ((!cut.IsZero() && started < cut.UnixNano()) || position >= r.RunsMax) {
				runIDs = append(runIDs, n)
			}
			position++
		}
		rows.Close()
		for _, runID := range runIDs {
			if _, e = db.ExecContext(ctx, `DELETE FROM run_output WHERE run_id=?`, runID); e != nil {
				return e
			}
			if _, e = db.ExecContext(ctx, `DELETE FROM run_metrics WHERE run_id=?`, runID); e != nil {
				return e
			}
			if _, e = db.ExecContext(ctx, `DELETE FROM transitions WHERE run_id=?`, runID); e != nil {
				return e
			}
			if _, e = db.ExecContext(ctx, `DELETE FROM runs WHERE run_id=?`, runID); e != nil {
				return e
			}
		}
	}
	if r.OutputAge > 0 {
		cut := time.Now().Add(-r.OutputAge).UnixNano()
		_, e = db.ExecContext(ctx, `DELETE FROM run_output WHERE run_id IN (SELECT r.run_id FROM runs r WHERE r.started_at<? AND r.run_id NOT IN (SELECT l.run_id FROM latest l) AND r.run_id NOT IN (SELECT r2.run_id FROM runs r2 WHERE r2.check_id=r.check_id ORDER BY r2.started_at DESC LIMIT ?))`, cut, maxInt(1, r.OutputMin))
		if e != nil {
			return e
		}
	}
	if r.TransitionsAge > 0 {
		if _, e = db.ExecContext(ctx, `DELETE FROM transitions WHERE at<?`, time.Now().Add(-r.TransitionsAge).UnixNano()); e != nil {
			return e
		}
	}
	if r.EventsAge > 0 {
		if _, e = db.ExecContext(ctx, `DELETE FROM daemon_events WHERE at<?`, time.Now().Add(-r.EventsAge).UnixNano()); e != nil {
			return e
		}
	}
	// Retain the logical ledger while it may still be useful for audit, then
	// remove old rows only after all foreign-keyed run history is gone.
	if _, e = db.ExecContext(ctx, `DELETE FROM checks WHERE deleted_at IS NOT NULL AND deleted_at<? AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.check_id=checks.check_id)`, time.Now().Add(-30*24*time.Hour).UnixNano()); e != nil {
		return e
	}
	if _, e = db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); e != nil {
		return e
	}
	_, e = db.ExecContext(ctx, "PRAGMA incremental_vacuum(256)")
	return e
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TryLease(ctx context.Context, db *sql.DB, id string, until time.Time) (bool, error) {
	tx, e := db.BeginTx(ctx, &sql.TxOptions{})
	if e != nil {
		return false, e
	}
	var old int64
	e = tx.QueryRowContext(ctx, `SELECT lease_until FROM leases WHERE check_id=?`, id).Scan(&old)
	if e != nil && e != sql.ErrNoRows {
		tx.Rollback()
		return false, e
	}
	now := time.Now().UnixNano()
	if e == nil && old > now {
		tx.Rollback()
		return false, nil
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO leases(check_id,lease_until) VALUES(?,?) ON CONFLICT(check_id) DO UPDATE SET lease_until=excluded.lease_until`, id, until.UnixNano())
	if e != nil {
		tx.Rollback()
		return false, e
	}
	return true, tx.Commit()
}
func ReleaseLease(ctx context.Context, db *sql.DB, id string) error {
	_, e := db.ExecContext(ctx, `DELETE FROM leases WHERE check_id=?`, id)
	return e
}
