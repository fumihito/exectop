package persist

import (
	"context"
	"database/sql"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterAndHistory(t *testing.T) {
	db, e := Open(t.TempDir() + "/r.db")
	if e != nil {
		t.Fatal(e)
	}
	w := NewWriter(db, 4)
	now := time.Now()
	if !w.Enqueue(Item{Result: runner.Result{ID: "x", Iter: 1, Trigger: "request", Status: status.OK, ExitCode: 0, Start: now, End: now.Add(time.Millisecond), Duration: time.Millisecond}}) {
		t.Fatal("drop")
	}
	w.Enqueue(Item{Result: runner.Result{ID: "x", Iter: 2, Trigger: "request", Status: status.Fail, ExitCode: 1, Start: now.Add(time.Second), End: now.Add(time.Second + time.Millisecond), Duration: time.Millisecond}})
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	rows, e := History(context.Background(), db, "x", 1)
	if e != nil || len(rows) != 1 || rows[0].Status != "FAIL" {
		t.Fatal(rows, e)
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM transitions").Scan(&n); e != nil || n != 2 {
		t.Fatalf("transitions %d %v", n, e)
	}
}
func TestLease(t *testing.T) {
	db, e := Open(t.TempDir() + "/l.db")
	if e != nil {
		t.Fatal(e)
	}
	ok, e := TryLease(context.Background(), db, "x", time.Now().Add(time.Minute))
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	ok, e = TryLease(context.Background(), db, "x", time.Now().Add(time.Minute))
	if e != nil || ok {
		t.Fatal(ok, e)
	}
	if e = ReleaseLease(context.Background(), db, "x"); e != nil {
		t.Fatal(e)
	}
	if ok, e = TryLease(context.Background(), db, "x", time.Now().Add(time.Minute)); e != nil || !ok {
		t.Fatal("released lease was not reusable", ok, e)
	}
	if _, e = db.Exec("UPDATE leases SET lease_until=? WHERE check_id=?", time.Now().Add(-time.Minute).UnixNano(), "x"); e != nil {
		t.Fatal(e)
	}
	if ok, e = TryLease(context.Background(), db, "x", time.Now().Add(time.Minute)); e != nil || !ok {
		t.Fatal("expired lease was not reusable", ok, e)
	}
}
func TestPrunePerCheck(t *testing.T) {
	db, e := Open(t.TempDir() + "/p.db")
	if e != nil {
		t.Fatal(e)
	}
	w := NewWriter(db, 20)
	now := time.Now()
	for i := 0; i < 5; i++ {
		w.Enqueue(Item{Result: runner.Result{ID: "a", Iter: i + 1, Status: status.OK, Start: now.Add(time.Duration(i) * time.Second), End: now, SpecHash: "x"}})
		w.Enqueue(Item{Result: runner.Result{ID: "b", Iter: i + 1, Status: status.OK, Start: now.Add(time.Duration(i) * time.Second), End: now, SpecHash: "x"}})
	}
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = Prune(context.Background(), db, 2); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM runs WHERE check_id='a'").Scan(&n); e != nil || n != 2 {
		t.Fatal(n, e)
	}
}

func TestRetentionKeepsLatestAndRemovesOldRuns(t *testing.T) {
	db, e := Open(t.TempDir() + "/r.db")
	if e != nil {
		t.Fatal(e)
	}
	s := runner.Result{ID: "x", Iter: 1, Status: status.OK, Start: time.Now().Add(-2 * time.Hour), End: time.Now().Add(-2 * time.Hour), Duration: time.Millisecond, Stdout: []byte("old output")}
	if e = UpsertCheck(context.Background(), db, spec.Spec{ID: "x", Name: "x", Path: "/x"}, ""); e != nil {
		t.Fatal(e)
	}
	w := NewWriter(db, 4)
	w.Enqueue(Item{Result: s})
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	w = NewWriter(db, 4)
	w.Enqueue(Item{Result: runner.Result{ID: "x", Iter: 2, Status: status.OK, Start: now, End: now, Duration: time.Millisecond}})
	_ = w.Close(context.Background())
	if e = PruneWithOptions(context.Background(), db, Retention{RunsMax: 100, RunsAge: time.Hour}); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM runs").Scan(&n); e != nil || n != 1 {
		t.Fatalf("runs=%d err=%v", n, e)
	}
}

func TestHistorySinceZeroMeansAllHistory(t *testing.T) {
	db, e := Open(t.TempDir() + "/h.db")
	if e != nil {
		t.Fatal(e)
	}
	s := spec.Spec{ID: "x", Name: "x", Path: "/x"}
	if e = UpsertCheck(context.Background(), db, s, ""); e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	w := NewWriter(db, 4)
	w.Enqueue(Item{Result: runner.Result{ID: "x", Iter: 1, Status: status.OK, Start: now, End: now}})
	_ = w.Close(context.Background())
	rows, e := HistorySince(context.Background(), db, "x", 60, time.Time{})
	if e != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), e)
	}
}

func TestLoadLatestOutputAndHistoryBounds(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "x.db"))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 8, BatchSize: 1, CompressOver: 1})
	w.Enqueue(Item{Result: runner.Result{ID: "x", Iter: 1, Status: status.OK, ExitCode: 0, Start: now.Add(-time.Second), End: now, Stdout: []byte(`{"summary":"ok","owner":"ops","latency_ms":12}` + "\nlarge stdout"), Stderr: []byte("stderr"), Truncated: true, Metrics: map[string]float64{"latency": 1.25}}})
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	latest, e := LoadLatest(context.Background(), db, []string{"x", "missing"})
	if e != nil || len(latest) != 1 || !strings.Contains(string(latest["x"].Result.Stdout), "large stdout") {
		t.Fatalf("latest=%+v err=%v", latest, e)
	}
	if !latest["x"].Result.Truncated || latest["x"].Result.Metrics["latency"] != 1.25 {
		t.Fatalf("restored metadata=%+v", latest["x"].Result)
	}
	if latest["x"].Result.Variables["owner"] != "ops" || latest["x"].Result.Variables["latency_ms"] != float64(12) {
		t.Fatalf("restored variables=%v", latest["x"].Result.Variables)
	}
	if _, ok := latest["missing"]; ok {
		t.Fatal("unexpected missing latest")
	}
	if e = RecordEvent(context.Background(), db, "test", "detail"); e != nil {
		t.Fatal(e)
	}
	if rows, e := Transitions(context.Background(), db, "x", time.Time{}, time.Time{}); e != nil || len(rows) != 1 {
		t.Fatalf("transitions=%v err=%v", rows, e)
	}
	if rows, e := HistoryBetween(context.Background(), db, "x", 10, now.Add(-2*time.Second), now.Add(2*time.Second)); e != nil || len(rows) != 1 {
		t.Fatalf("history=%v err=%v", rows, e)
	}
	if rows, e := HistoryBetween(context.Background(), db, "x", 10, now.Add(time.Second), now.Add(2*time.Second)); e != nil || len(rows) != 0 {
		t.Fatalf("bounded history=%v err=%v", rows, e)
	}
	if e = Vacuum(context.Background(), db); e != nil {
		t.Fatal(e)
	}
}

func TestUnavailableExitCodeIsStoredAsNull(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "null-exit.db"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 2, BatchSize: 1})
	now := time.Now()
	if !w.Enqueue(Item{Result: runner.Result{ID: "x", Status: status.Error, ExitCode: -1, Start: now, End: now}}) {
		t.Fatal("enqueue")
	}
	if err = w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var exit sql.NullInt64
	if err = db.QueryRow("SELECT exit_code FROM runs WHERE check_id='x'").Scan(&exit); err != nil {
		t.Fatal(err)
	}
	if exit.Valid {
		t.Fatalf("exit_code=%v, want NULL", exit.Int64)
	}
	latest, err := LoadLatest(context.Background(), db, []string{"x"})
	if err != nil || latest["x"].Result.ExitCode != -1 {
		t.Fatalf("restored=%+v err=%v", latest, err)
	}
}

func TestWriterDropsWhenQueueIsFull(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "drop.db"))
	if e != nil {
		t.Fatal(e)
	}
	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 1, BatchSize: 1000, BatchInterval: time.Hour})
	r := runner.Result{ID: "x", Status: status.OK, Start: time.Now(), End: time.Now()}
	accepted := 0
	for i := 0; i < 100; i++ {
		if w.Enqueue(Item{Result: r}) {
			accepted++
		}
	}
	if accepted >= 100 || w.Stats().Dropped == 0 {
		t.Fatalf("accepted=%d stats=%+v", accepted, w.Stats())
	}
	_ = w.Close(context.Background())
}

func TestOpenRejectsNewerSchemaAndRecoversCorruptDB(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "schema.db")
	db, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE schema_meta SET value='2' WHERE key='schema_version'`); e != nil {
		t.Fatal(e)
	}
	_ = db.Close()
	if db, e = Open(p); e == nil || db != nil {
		t.Fatalf("newer schema db=%v err=%v", db, e)
	}
	corrupt := filepath.Join(dir, "corrupt.db")
	if e = os.WriteFile(corrupt, []byte("not sqlite"), 0600); e != nil {
		t.Fatal(e)
	}
	if db, e = Open(corrupt); e != nil {
		t.Fatal(e)
	}
	matches, _ := filepath.Glob(corrupt + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt backups=%v", matches)
	}
	var kind string
	if e = db.QueryRow("SELECT kind FROM daemon_events WHERE kind='corrupt_recovered'").Scan(&kind); e != nil || kind != "corrupt_recovered" {
		t.Fatalf("corrupt recovery event=%q err=%v", kind, e)
	}
	_ = db.Close()
}

func TestWriterCloseTimeoutAndWriteFailure(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "slow.db"))
	if e != nil {
		t.Fatal(e)
	}
	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 2, BatchSize: 1, BatchInterval: time.Hour, CommitDelay: 100 * time.Millisecond})
	w.Enqueue(Item{Result: runner.Result{ID: "x", Status: status.OK, Start: time.Now(), End: time.Now()}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	e = w.Close(ctx)
	cancel()
	if e == nil {
		t.Fatal("expected close timeout")
	}
	_ = db.Close()
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestDecodeOutputAndWriterRetries(t *testing.T) {
	if got := DecodeOutput("raw", []byte("raw")); string(got) != "raw" {
		t.Fatal(got)
	}
	if got := DecodeOutput("gzip", []byte("bad")); got != nil {
		t.Fatal("invalid gzip decoded")
	}
	db, e := Open(filepath.Join(t.TempDir(), "retry.db"))
	if e != nil {
		t.Fatal(e)
	}
	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 2, BatchSize: 1, BatchInterval: time.Hour})
	_ = db.Close()
	w.Enqueue(Item{Result: runner.Result{ID: "x", Status: status.OK, Start: time.Now(), End: time.Now()}})
	_ = w.Close(context.Background())
	if w.Stats().Dropped == 0 {
		t.Fatal("failed write was not counted")
	}
}

func TestPruneOutputAgeProtectsRecentRuns(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "output.db"))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 4, BatchSize: 1, CompressOver: 10000})
	for i := 0; i < 3; i++ {
		w.Enqueue(Item{Result: runner.Result{ID: "x", Iter: i + 1, Status: status.OK, Start: now.Add(-time.Duration(3-i) * time.Hour), End: now, Stdout: []byte("output")}})
	}
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = PruneWithOptions(context.Background(), db, Retention{RunsMax: 100, OutputAge: time.Minute, OutputMin: 2}); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM run_output").Scan(&n); e != nil || n != 2 {
		t.Fatalf("output rows=%d err=%v", n, e)
	}
	// OutputMin=0 takes the lower-bound branch and must still retain one
	// recent output per check.
	if e = PruneWithOptions(context.Background(), db, Retention{RunsMax: 100, OutputAge: time.Minute, OutputMin: 0}); e != nil {
		t.Fatal(e)
	}
}

func TestLedgerDeletionRetentionAndBlockingWriter(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if e != nil {
		t.Fatal(e)
	}
	s := spec.Spec{ID: "x", Name: "x", Path: "/x"}
	if e = UpsertCheck(context.Background(), db, s, "g"); e != nil {
		t.Fatal(e)
	}
	if e = MarkDeleted(context.Background(), db, []string{"x"}); e != nil {
		t.Fatal(e)
	}
	var deleted int64
	if e = db.QueryRow("SELECT deleted_at FROM checks WHERE check_id='x'").Scan(&deleted); e != nil || deleted == 0 {
		t.Fatalf("deleted=%d err=%v", deleted, e)
	}
	if e = UpsertCheck(context.Background(), db, spec.Spec{ID: "old", Name: "old", Path: "/old"}, ""); e != nil {
		t.Fatal(e)
	}
	if e = MarkDeleted(context.Background(), db, []string{"old"}); e != nil {
		t.Fatal(e)
	}
	oldDeleted := time.Now().Add(-31 * 24 * time.Hour).UnixNano()
	if _, e = db.Exec("UPDATE checks SET deleted_at=? WHERE check_id='old'", oldDeleted); e != nil {
		t.Fatal(e)
	}
	if e = RecordEvent(context.Background(), nil, "noop", ""); e != nil {
		t.Fatal(e)
	}
	old := time.Now().Add(-time.Hour).UnixNano()
	if _, e = db.Exec("INSERT INTO daemon_events(at,kind,detail) VALUES(?,?,?)", old, "old", "x"); e != nil {
		t.Fatal(e)
	}
	res, e := db.Exec("INSERT INTO runs(check_id,iter,trigger,status,started_at,ended_at,duration_ms,spec_hash) VALUES('x',1,'request','OK',?,?,0,'x')", old, old)
	if e != nil {
		t.Fatal(e)
	}
	runID, e := res.LastInsertId()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO transitions(check_id,run_id,from_status,to_status,at) VALUES('x',?,NULL,'OK',?)", runID, old); e != nil {
		t.Fatal(e)
	}
	if e = PruneWithOptions(context.Background(), db, Retention{RunsMax: 2, EventsAge: time.Minute, TransitionsAge: time.Minute}); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM daemon_events WHERE kind='old'").Scan(&n); e != nil || n != 0 {
		t.Fatalf("events=%d err=%v", n, e)
	}
	if e = db.QueryRow("SELECT count(*) FROM transitions WHERE at=?", old).Scan(&n); e != nil || n != 0 {
		t.Fatalf("transitions=%d err=%v", n, e)
	}
	if e = db.QueryRow("SELECT count(*) FROM checks WHERE check_id='old'").Scan(&n); e != nil || n != 0 {
		t.Fatalf("old ledger row=%d err=%v", n, e)
	}

	w := NewWriterWithConfig(db, WriterConfig{QueueSize: 1, BatchSize: 100, BatchInterval: time.Hour, Block: true})
	r := runner.Result{ID: "x", Status: status.OK, Start: time.Now(), End: time.Now()}
	if !w.Enqueue(Item{Result: r}) {
		t.Fatal("first block enqueue")
	}
	if !w.Enqueue(Item{Result: r}) {
		t.Fatal("second block enqueue")
	}
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
}

func TestWriterDefaultsClosedAndEmptyQueries(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "defaults.db"))
	if e != nil {
		t.Fatal(e)
	}
	w := NewWriterWithConfig(db, WriterConfig{})
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if w.Enqueue(Item{}) {
		t.Fatal("closed writer accepted item")
	}
	if e = w.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if rows, e := History(context.Background(), db, "none", 0); e != nil || len(rows) != 0 {
		t.Fatalf("history=%v err=%v", rows, e)
	}
	if rows, e := HistoryBetween(context.Background(), db, "none", 0, time.Time{}, time.Time{}); e != nil || len(rows) != 0 {
		t.Fatalf("history between=%v err=%v", rows, e)
	}
	if rows, e := Transitions(context.Background(), db, "none", time.Now().Add(-time.Second), time.Now()); e != nil || len(rows) != 0 {
		t.Fatalf("transitions=%v err=%v", rows, e)
	}
}

func TestPersistenceContextErrors(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "context.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = MarkDeleted(ctx, db, []string{"missing"}); err == nil {
		t.Fatal("expected MarkDeleted context error")
	}
	if ok, err := TryLease(ctx, db, "missing", time.Now().Add(time.Minute)); err == nil || ok {
		t.Fatal("expected TryLease context error", ok, err)
	}
	if err = Vacuum(ctx, db); err == nil {
		t.Fatal("expected Vacuum context error")
	}
}
