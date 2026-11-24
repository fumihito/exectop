package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"exectop/src/coordinator"
	"exectop/src/discover"
	"exectop/src/metrics"
	"exectop/src/persist"
	"exectop/src/rpc"
	"exectop/src/runner"
	"exectop/src/sched"
	"exectop/src/spec"
	"exectop/src/status"
	"exectop/src/version"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Daemon struct {
	Root, Socket, DBPath string
	coord                *coordinator.Coordinator
	store                *coordinator.Store
	writer               *persist.Writer
	db                   *sql.DB
	ln                   net.Listener
	stop                 chan struct{}
	once                 sync.Once
	started              time.Time
	scheduler            *sched.Scheduler
	lock                 *os.File
	background           bool
	idleExit             time.Duration
	pruneInterval        time.Duration
	rescanInterval       time.Duration
	retention            persist.Retention
	discoverOptions      discover.Options
	configPath           string
	stateRoot            string
	noState              bool
	inheritEnv           bool
	warnings             []discover.Warning
	deletedPending       map[string]bool
	stateMu              sync.RWMutex
	runningPath          string
	lastActivity         atomic.Int64
	subscribers          atomic.Int32
	idleStarted          atomic.Bool
	pruneStarted         atomic.Bool
	pruneWake            chan struct{}
	metricsMu            sync.Mutex
	rpcMetrics           map[string]*metrics.Histogram
	logMu                sync.Mutex
	logFile              *os.File
	logPath              string
	exeMtime             int64
	activeMu             sync.Mutex
	active               map[int]context.CancelFunc
	activeNext           int
	rescanMu             sync.Mutex
	fingerprints         map[string]string
}
type Options struct {
	FollowSymlinks bool
	AllowUnsafe    bool
	ConfigPath     string
	Rescan         time.Duration
	StateDir       string
	NoState        bool
	Concurrency    int
	History        int
}

func defaultRetention() persist.Retention {
	return persist.Retention{RunsMax: 1000, RunsAge: 168 * time.Hour, OutputAge: 24 * time.Hour, OutputMin: 20, TransitionsAge: 2160 * time.Hour, EventsAge: 720 * time.Hour}
}

type requestFilter struct {
	IDs   []string `json:"ids"`
	Tag   string   `json:"tag"`
	Group string   `json:"group"`
	Match string   `json:"match"`
}

func (d *Daemon) filteredEntries(raw json.RawMessage) ([]coordinator.Entry, error) {
	var p struct {
		Filter requestFilter `json:"filter"`
		requestFilter
	}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid filter: %w", err)
		}
	}
	f := p.Filter
	if len(f.IDs) == 0 && len(p.IDs) > 0 {
		f.IDs = p.IDs
	}
	if f.Tag == "" {
		f.Tag = p.Tag
	}
	if f.Group == "" {
		f.Group = p.Group
	}
	if f.Match == "" {
		f.Match = p.Match
	}
	var re *regexp.Regexp
	var err error
	if f.Match != "" {
		re, err = regexp.Compile(f.Match)
		if err != nil {
			return nil, fmt.Errorf("invalid filter match: %w", err)
		}
	}
	wanted := map[string]bool{}
	for _, id := range f.IDs {
		wanted[id] = true
	}
	out := make([]coordinator.Entry, 0)
	for _, e := range d.store.Snapshot() {
		if len(wanted) > 0 && !wanted[e.Spec.ID] {
			continue
		}
		if f.Group != "" && groupOfID(e.Spec.ID) != f.Group {
			continue
		}
		if f.Tag != "" && !containsTag(e.Spec.Tags, f.Tag) {
			continue
		}
		if re != nil && !re.MatchString(e.Spec.ID) && !re.MatchString(e.Spec.Name) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.ID < out[j].Spec.ID })
	return out, nil
}

func groupOfID(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return ""
}
func containsTag(tags []string, tag string) bool {
	for _, v := range tags {
		if v == tag {
			return true
		}
	}
	return false
}

func RootHash(root string) string {
	p, e := filepath.Abs(root)
	if e == nil {
		p, _ = filepath.EvalSymlinks(p)
	}
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:])[:16]
}
func Paths(root string) (string, string) {
	h := RootHash(root)
	sockDir := filepath.Join(os.TempDir(), fmt.Sprintf("exectop-%d", os.Getuid()))
	_ = os.MkdirAll(sockDir, 0700)
	_ = os.Chmod(sockDir, 0700)
	sock := filepath.Join(sockDir, h+".sock")
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run != "" {
		d := filepath.Join(run, "exectop")
		if e := os.MkdirAll(d, 0700); e == nil {
			_ = os.Chmod(d, 0700)
			sock = filepath.Join(d, h+".sock")
		}
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if home, e := os.UserHomeDir(); e == nil {
			state = filepath.Join(home, ".local", "state")
		}
	}
	if state == "" || os.MkdirAll(filepath.Join(state, "exectop", h), 0700) != nil {
		state = os.TempDir()
	} else {
		_ = os.Chmod(filepath.Join(state, "exectop", h), 0700)
	}
	return sock, filepath.Join(state, "exectop", h, "exectop.db")
}
func New(root string) (*Daemon, error) {
	return NewWithOptions(root, Options{})
}
func NewWithOptions(root string, opt Options) (*Daemon, error) {
	f, warnings, e := discover.Discover(root, discover.Options{FollowSymlinks: opt.FollowSymlinks, AllowUnsafe: opt.AllowUnsafe})
	if e != nil {
		return nil, e
	}
	sock, dbp := Paths(root)
	db, e := persist.Open(dbp)
	if e != nil {
		return nil, e
	}
	st := coordinator.NewStore()
	if opt.History > 0 {
		st.SetHistoryLimit(opt.History)
	}
	d := &Daemon{Root: root, Socket: sock, DBPath: dbp, db: db, store: st, stop: make(chan struct{}), started: time.Now(), rpcMetrics: map[string]*metrics.Histogram{}, discoverOptions: discover.Options{FollowSymlinks: opt.FollowSymlinks, AllowUnsafe: opt.AllowUnsafe}, configPath: opt.ConfigPath, stateRoot: opt.StateDir, noState: opt.NoState, warnings: warnings, deletedPending: map[string]bool{}, active: map[int]context.CancelFunc{}, rescanInterval: opt.Rescan, fingerprints: map[string]string{}, pruneInterval: time.Hour, retention: defaultRetention(), pruneWake: make(chan struct{}, 1)}
	d.runningPath = filepath.Join(filepath.Dir(dbp), "daemon.running")
	d.logPath = filepath.Join(filepath.Dir(dbp), "daemon.log")
	if exe, ee := os.Executable(); ee == nil {
		if st, ee := os.Stat(exe); ee == nil {
			d.exeMtime = st.ModTime().UnixNano()
		}
	}
	if d.configPath == "" {
		d.configPath = filepath.Join(root, "exectop.toml")
	}
	var globalConfigErr error
	if _, ge := spec.LoadDirectoryConfig(d.configPath); ge != nil {
		globalConfigErr = ge
	}
	d.lastActivity.Store(time.Now().UnixNano())
	if gc, ge := spec.LoadDirectoryConfig(d.configPath); ge == nil {
		cfg := gc.Sections["daemon"]
		d.background = cfg["background"] == "true"
		if v := cfg["idle_exit"]; v != "" && v != "0" {
			d.idleExit, _ = time.ParseDuration(v)
		}
	}
	d.coord = coordinator.New(st, func(ctx context.Context, s spec.Spec, m runner.Meta) runner.Result {
		m.StateDir = d.stateDir(s.ID)
		m.RunningFile = d.runningPath
		return runner.Run(ctx, s, m)
	})
	if gc, ge := spec.LoadDirectoryConfig(d.configPath); ge == nil {
		con := gc.Defaults["concurrency"]
		if gc.Sections["global"] != nil {
			con = gc.Sections["global"]["concurrency"]
			d.inheritEnv = gc.Sections["global"]["inherit_env"] == "true"
			if n, e := strconv.Atoi(gc.Sections["global"]["history"]); e == nil && n > 0 {
				st.SetHistoryLimit(n)
			}
		}
		if n, e := strconv.Atoi(con); e == nil && n > 0 {
			d.coord.SetConcurrency(n)
		}
	}
	if opt.Concurrency > 0 {
		d.coord.SetConcurrency(opt.Concurrency)
	}
	for _, x := range f {
		m, he := spec.ParseHeader(x.Path)
		if he != nil {
			s := spec.Default(x)
			s.InvalidReason = he.Error()
			d.coord.Add(s)
			d.fingerprints[s.ID] = rescanFingerprint(s)
			_ = persist.UpsertCheck(context.Background(), db, s, x.Group)
			continue
		}
		layers, ce := spec.ChainWithHeader(root, x.Path, x.ID, m)
		if ce != nil {
			s := spec.Default(x)
			s.InvalidReason = ce.Error()
			d.coord.Add(s)
			d.fingerprints[s.ID] = rescanFingerprint(s)
			_ = persist.UpsertCheck(context.Background(), db, s, x.Group)
			continue
		}
		global := map[string]string{}
		if globalConfigErr != nil {
			s := spec.Default(x)
			s.InvalidReason = globalConfigErr.Error()
			d.coord.Add(s)
			d.fingerprints[s.ID] = rescanFingerprint(s)
			_ = persist.UpsertCheck(context.Background(), db, s, x.Group)
			continue
		}
		if gc, ge := spec.LoadDirectoryConfig(d.configPath); ge == nil {
			for k, v := range gc.Defaults {
				global[k] = v
			}
			for k, v := range gc.Sections["global"] {
				global[k] = v
			}
		}
		s, e := spec.Resolve(x, layers, global, spec.Overrides{AllowUnsafe: opt.AllowUnsafe})
		if e == nil {
			d.coord.Add(s)
			d.fingerprints[s.ID] = rescanFingerprint(s)
			_ = persist.UpsertCheck(context.Background(), db, s, x.Group)
		} else {
			s = spec.Default(x)
			s.InvalidReason = e.Error()
			d.coord.Add(s)
			d.fingerprints[s.ID] = rescanFingerprint(s)
			_ = persist.UpsertCheck(context.Background(), db, s, x.Group)
		}
	}
	ids := make([]string, 0, len(f))
	for _, x := range f {
		ids = append(ids, x.ID)
	}
	if rows, e := persist.LoadLatest(context.Background(), db, ids); e == nil {
		for id, v := range rows {
			st.Restore(id, v.Result)
		}
	}
	for _, id := range ids {
		if rows, e := persist.History(context.Background(), db, id, 60); e == nil {
			points := make([]coordinator.HistoryPoint, 0, len(rows))
			for i := len(rows) - 1; i >= 0; i-- {
				exit := -1
				if rows[i].ExitCode.Valid {
					exit = int(rows[i].ExitCode.Int64)
				}
				points = append(points, coordinator.HistoryPoint{Status: status.Parse(rows[i].Status), Duration: rows[i].Duration, Start: rows[i].Started, ExitCode: exit, Summary: rows[i].Summary})
			}
			st.SetHistory(id, points)
		}
	}
	wcfg := persist.WriterConfig{QueueSize: 4096, BatchSize: 200, BatchInterval: 200 * time.Millisecond, CompressOver: 4096}
	if gc, ge := spec.LoadDirectoryConfig(d.configPath); ge == nil {
		if pc := gc.Sections["persist"]; pc != nil {
			if n, e := strconv.Atoi(pc["queue_size"]); e == nil && n > 0 {
				wcfg.QueueSize = n
			}
			if n, e := strconv.Atoi(pc["batch_size"]); e == nil && n > 0 {
				wcfg.BatchSize = n
			}
			if v := pc["batch_interval"]; v != "" {
				if d, e := time.ParseDuration(v); e == nil {
					wcfg.BatchInterval = d
				}
			}
			if v := pc["compress_over"]; v != "" {
				if n, e := parseSize(v); e == nil {
					wcfg.CompressOver = n
				}
			}
			wcfg.Block = pc["block"] == "true"
			if v := pc["prune_interval"]; v != "" {
				d.pruneInterval, _ = time.ParseDuration(v)
			}
		}
		if rc := gc.Sections["persist.retention"]; rc != nil {
			if v := rc["runs_max"]; v != "" {
				d.retention.RunsMax, _ = strconv.Atoi(v)
			}
			if v := rc["output_min"]; v != "" {
				d.retention.OutputMin, _ = strconv.Atoi(v)
			}
			if v := rc["runs"]; v != "" {
				d.retention.RunsAge, _ = time.ParseDuration(v)
			}
			if v := rc["output"]; v != "" {
				d.retention.OutputAge, _ = time.ParseDuration(v)
			}
			if v := rc["transitions"]; v != "" {
				d.retention.TransitionsAge, _ = time.ParseDuration(v)
			}
			if v := rc["events"]; v != "" {
				d.retention.EventsAge, _ = time.ParseDuration(v)
			}
		}
	}
	if v := os.Getenv("EXECTOP_BENCH_COMMIT_DELAY"); v != "" {
		if delay, e := time.ParseDuration(v); e == nil {
			wcfg.CommitDelay = delay
		}
	}
	d.writer = persist.NewWriterWithConfig(db, wcfg)
	d.scheduler = sched.New(d.coord)
	d.coord.SetResultHook(func(r runner.Result) {
		d.writer.Enqueue(persist.Item{Result: r})
		d.stateMu.Lock()
		pending := d.deletedPending[r.ID]
		if pending {
			delete(d.deletedPending, r.ID)
		}
		d.stateMu.Unlock()
		if pending {
			_ = persist.MarkDeleted(context.Background(), d.db, []string{r.ID})
			d.store.Remove(r.ID)
		}
	})
	return d, nil
}
func cleanupRunning(path string) {
	b, e := os.ReadFile(path)
	if e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if pid, e := strconv.Atoi(strings.TrimSpace(line)); e == nil && pid > 0 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	}
	_ = os.WriteFile(path, nil, 0600)
}
func parseSize(v string) (int, error) {
	v = strings.TrimSpace(v)
	m := 1
	for suffix, factor := range map[string]int{"GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024, "Gi": 1024 * 1024 * 1024, "Mi": 1024 * 1024, "Ki": 1024, "G": 1024 * 1024 * 1024, "M": 1024 * 1024, "K": 1024} {
		if len(v) > len(suffix) && strings.EqualFold(v[len(v)-len(suffix):], suffix) {
			m = factor
			v = v[:len(v)-len(suffix)]
			break
		}
	}
	n, e := strconv.Atoi(strings.TrimSpace(v))
	return n * m, e
}
func (d *Daemon) Start() error {
	lf, e := os.OpenFile(filepath.Join(filepath.Dir(d.DBPath), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	if e = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lf.Close()
		return fmt.Errorf("daemon already running: %w", e)
	}
	d.lock = lf
	if e = writeLockInfo(lf, d.started); e != nil {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		d.lock = nil
		return e
	}
	cleanupRunning(d.runningPath)
	_ = os.Remove(d.Socket)
	if e := os.MkdirAll(filepath.Dir(d.Socket), 0700); e != nil {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		return e
	}
	ln, e := net.Listen("unix", d.Socket)
	if e != nil {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		return e
	}
	d.ln = ln
	_ = os.Chmod(d.Socket, 0600)
	if e = d.openLog(); e != nil {
		_ = ln.Close()
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
		return e
	}
	d.writeLog("INFO", "daemon started", map[string]any{"pid": os.Getpid(), "root": d.Root})
	_ = persist.RecordEvent(context.Background(), d.db, "start", fmt.Sprintf("pid=%d", os.Getpid()))
	d.stateMu.RLock()
	inheritEnv := d.inheritEnv
	background := d.background
	idleExit := d.idleExit
	pruneInterval := d.pruneInterval
	d.stateMu.RUnlock()
	d.scheduler.SetInheritEnv(inheritEnv)
	if background {
		d.scheduler.Start(context.Background(), d.specs())
	}
	if idleExit > 0 {
		d.startIdleMonitor()
	}
	go d.rootMonitor()
	if pruneInterval > 0 {
		d.startPruneLoop()
	}
	if d.rescanInterval > 0 {
		go d.rescanLoop()
	}
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				select {
				case <-d.stop:
					return
				default:
					continue
				}
			}
			if !sameUser(c) {
				d.writeLog("WARN", "rejected peer credentials", nil)
				_ = c.Close()
				continue
			}
			go rpc.ServeStream(c, d.handle, d.stream)
		}
	}()
	return nil
}
func (d *Daemon) stream(q rpc.Request) (<-chan rpc.Response, func(), bool) {
	if q.Method == "run" {
		var p struct {
			Wait *bool `json:"wait"`
		}
		if e := json.Unmarshal(q.Params, &p); e != nil || (p.Wait != nil && !*p.Wait) {
			return nil, nil, false
		}
		entries, e := d.filteredEntries(q.Params)
		if e != nil {
			return nil, nil, false
		}
		var rp struct {
			Force      bool          `json:"force"`
			MaxAge     time.Duration `json:"max_age"`
			InheritEnv bool          `json:"inherit_env"`
			Timeout    time.Duration `json:"timeout"`
			Interval   time.Duration `json:"interval"`
		}
		_ = json.Unmarshal(q.Params, &rp)
		out := make(chan rpc.Response, len(entries)+1)
		go func() {
			var wg sync.WaitGroup
			for _, entry := range entries {
				id := entry.Spec.ID
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, done := d.beginRequest()
					defer done()
					v, err := d.coord.Request(ctx, id, coordinator.Freshness{Force: rp.Force, MaxAge: rp.MaxAge, InheritEnv: rp.InheritEnv, Timeout: rp.Timeout, Interval: rp.Interval}, runner.Trigger("request"))
					if err != nil {
						out <- rpc.Response{ID: q.ID, Error: &rpc.RPCError{Code: "run_error", Message: err.Error()}, Final: false}
						return
					}
					out <- rpc.Response{ID: q.ID, Result: v, Final: false}
				}()
			}
			wg.Wait()
			out <- rpc.Response{ID: q.ID, Final: true}
			close(out)
		}()
		return out, func() {}, true
	}
	if q.Method != "subscribe" {
		return nil, nil, false
	}
	if _, err := d.filteredEntries(q.Params); err != nil {
		return nil, nil, false
	}
	var p struct {
		Splay    time.Duration `json:"splay"`
		Rescan   time.Duration `json:"rescan"`
		Interval time.Duration `json:"interval"`
		Timeout  time.Duration `json:"timeout"`
	}
	var envp struct {
		InheritEnv bool `json:"inherit_env"`
	}
	_ = json.Unmarshal(q.Params, &p)
	_ = json.Unmarshal(q.Params, &envp)
	d.scheduler.SetSplay(p.Splay)
	d.scheduler.SetInterval(p.Interval)
	d.scheduler.SetTimeout(p.Timeout)
	if envp.InheritEnv {
		d.scheduler.SetInheritEnv(true)
	} else {
		d.stateMu.RLock()
		inheritEnv := d.inheritEnv
		d.stateMu.RUnlock()
		d.scheduler.SetInheritEnv(inheritEnv)
	}
	d.scheduler.Start(context.Background(), d.specs())
	rescanStop := make(chan struct{})
	if p.Rescan > 0 && d.rescanInterval <= 0 {
		go func(interval time.Duration) {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					_ = d.Reload()
				case <-rescanStop:
					return
				case <-d.stop:
					return
				}
			}
		}(p.Rescan)
	}
	d.subscribers.Add(1)
	id, ch, cleanup := d.coord.Subscribe(128)
	out := make(chan rpc.Response, 128)
	entries, _ := d.filteredEntries(q.Params)
	allowed := make(map[string]bool, len(entries))
	runbooks := make(map[string]string, len(entries))
	for _, e := range entries {
		allowed[e.Spec.ID] = true
		runbooks[e.Spec.ID] = e.Spec.Runbook
	}
	go func() {
		for v := range ch {
			if v.State == "added" && v.Spec != nil {
				entries, _ := d.filteredEntries(q.Params)
				for _, entry := range entries {
					if entry.Spec.ID == v.Spec.ID {
						allowed[v.Spec.ID] = true
						runbooks[v.Spec.ID] = v.Spec.Runbook
						break
					}
				}
			}
			eventID := v.Result.ID
			if eventID == "" {
				eventID = v.ID
			}
			if eventID != "" && !allowed[eventID] {
				continue
			}
			var response rpc.Response
			if v.State != "" {
				result := runner.Result{ID: eventID, Runbook: runbooks[eventID], Status: status.Running, Start: time.Now()}
				if v.State == "changed" {
					if entry, ok := d.store.Get(eventID); ok {
						if entry.Latest != nil {
							result = *entry.Latest
						}
						result.ID = eventID
						result.Name = entry.Spec.Name
						result.Runbook = entry.Spec.Runbook
						result.SpecHash = entry.Spec.Hash
						result.Changed = true
						result.ChangedAt = time.Now()
					}
				}
				response = rpc.Response{ID: q.ID, Result: map[string]any{"state": v.State, "id": eventID, "result": result, "spec": v.Spec}, Final: false}
			} else {
				response = rpc.Response{ID: q.ID, Result: v, Final: false}
			}
			select {
			case out <- response:
			default:
				// A slow subscriber must not block result publication for the
				// daemon. The next snapshot/reconnect provides a fresh state.
			}
		}
		close(out)
	}()
	_ = id
	return out, func() {
		close(rescanStop)
		cleanup()
		d.stateMu.RLock()
		background := d.background
		d.stateMu.RUnlock()
		if d.subscribers.Add(-1) == 0 && !background {
			d.scheduler.Stop()
		}
	}, true
}
func sameUser(c net.Conn) bool {
	ok := false
	if x, yes := c.(interface {
		SyscallConn() (syscall.RawConn, error)
	}); yes {
		raw, e := x.SyscallConn()
		if e == nil {
			_ = raw.Control(func(fd uintptr) {
				u, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
				ok = e == nil && u.Uid == uint32(os.Getuid())
			})
		}
	}
	return ok
}
func (d *Daemon) handle(q rpc.Request) []rpc.Response {
	start := time.Now()
	out := d.handleInner(q)
	d.metricsMu.Lock()
	h := d.rpcMetrics[q.Method]
	if h == nil {
		h = &metrics.Histogram{}
		d.rpcMetrics[q.Method] = h
	}
	h.Observe(time.Since(start))
	d.metricsMu.Unlock()
	return out
}
func (d *Daemon) handleInner(q rpc.Request) []rpc.Response {
	d.lastActivity.Store(time.Now().UnixNano())
	switch q.Method {
	case "hello":
		var p struct {
			ProtoVersion int   `json:"proto_version"`
			ExeMtime     int64 `json:"exe_mtime"`
		}
		_ = json.Unmarshal(q.Params, &p)
		if p.ProtoVersion != 0 && p.ProtoVersion != 1 {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "proto_mismatch", Message: "unsupported protocol version"}, Final: true}}
		}
		if p.ExeMtime > 0 && d.exeMtime > 0 && p.ExeMtime < d.exeMtime {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "daemon_newer", Message: "daemon executable is newer"}, Final: true}}
		}
		if p.ExeMtime > 0 && d.exeMtime > 0 && p.ExeMtime > d.exeMtime {
			go func() { time.Sleep(10 * time.Millisecond); d.Stop() }()
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "client_newer", Message: "client executable is newer; restarting daemon"}, Final: true}}
		}
		return []rpc.Response{{ID: q.ID, Result: map[string]any{"daemon_version": version.Current, "proto_version": 1, "root": d.Root, "pid": os.Getpid(), "started_at": d.started, "db_path": d.DBPath, "exe_mtime": d.exeMtime}}}
	case "status":
		var p struct {
			Metrics bool `json:"metrics"`
		}
		_ = json.Unmarshal(q.Params, &p)
		running := 0
		for _, e := range d.store.Snapshot() {
			if e.Running {
				running++
			}
		}
		stats := persist.WriterStats{}
		if d.writer != nil {
			stats = d.writer.Stats()
		}
		overruns := map[string]int{}
		if d.scheduler != nil {
			overruns = d.scheduler.OverrunSnapshot()
		}
		result := map[string]any{"pid": os.Getpid(), "checks": len(d.store.Snapshot()), "running": running, "uptime_ms": time.Since(d.started).Milliseconds(), "queue_depth": stats.Depth, "queue_depth_max": stats.QueueDepthMax, "dropped": stats.Dropped, "last_commit_ms": stats.LastCommitMS, "scheduler_running": d.scheduler != nil && d.scheduler.Running(), "overruns": overruns, "goroutines": runtime.NumGoroutine()}
		if p.Metrics {
			d.metricsMu.Lock()
			mm := map[string]any{}
			for name, h := range d.rpcMetrics {
				mm[name] = h.Snapshot()
			}
			d.metricsMu.Unlock()
			result["metrics"] = mm
			for name, v := range metrics.SnapshotAll() {
				mm[name] = v
			}
		}
		return []rpc.Response{{ID: q.ID, Result: result}}
	case "list":
		entries, e := d.filteredEntries(q.Params)
		if e != nil {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "invalid_filter", Message: e.Error()}, Final: true}}
		}
		var a []any
		for _, e := range entries {
			a = append(a, e.Spec)
		}
		d.stateMu.RLock()
		warnings := append([]discover.Warning(nil), d.warnings...)
		d.stateMu.RUnlock()
		return []rpc.Response{{ID: q.ID, Result: map[string]any{"checks": a, "warnings": warnings}}}
	case "subscribe":
		entries, e := d.filteredEntries(q.Params)
		if e != nil {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "invalid_filter", Message: e.Error()}, Final: true}}
		}
		d.scheduler.Start(context.Background(), d.specs())
		var p struct {
			Snapshot *bool `json:"snapshot"`
		}
		_ = json.Unmarshal(q.Params, &p)
		if p.Snapshot != nil && !*p.Snapshot {
			return []rpc.Response{{ID: q.ID, Result: []any{}, Final: false}}
		}
		var a []any
		for _, e := range entries {
			changed := e.Latest != nil && e.Spec.Hash != "" && e.Spec.Hash != e.Latest.SpecHash
			a = append(a, map[string]any{"state": "snapshot", "id": e.Spec.ID, "status": latestStatus(e), "changed": changed})
		}
		return []rpc.Response{{ID: q.ID, Result: a, Final: false}}
	case "pause":
		d.scheduler.Stop()
		return []rpc.Response{{ID: q.ID, Result: map[string]bool{"paused": true}, Final: true}}
	case "resume":
		d.scheduler.Start(context.Background(), d.specs())
		return []rpc.Response{{ID: q.ID, Result: map[string]bool{"paused": false}, Final: true}}
	case "reload":
		return d.handle(rpc.Request{ID: q.ID, Method: "rescan"})
	case "rescan":
		d.rescanMu.Lock()
		defer d.rescanMu.Unlock()
		wasRunning := d.scheduler.Running()
		before := make(map[string]string, len(d.fingerprints))
		for id, fingerprint := range d.fingerprints {
			before[id] = fingerprint
		}
		f, warnings, e := discover.Discover(d.Root, d.discoverOptions)
		if e != nil {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "rescan_error", Message: e.Error()}, Final: true}}
		}
		global := map[string]string{}
		var globalErr error
		d.stateMu.RLock()
		oldBackground := d.background
		oldIdleExit := d.idleExit
		d.stateMu.RUnlock()
		d.stateMu.RLock()
		oldPruneInterval := d.pruneInterval
		d.stateMu.RUnlock()
		// Reload from a clean baseline so removing a setting restores the
		// documented built-in default instead of retaining stale state.
		d.stateMu.Lock()
		d.background = false
		d.idleExit = 0
		d.inheritEnv = false
		d.stateMu.Unlock()
		d.stateMu.Lock()
		d.pruneInterval = time.Hour
		d.retention = defaultRetention()
		d.stateMu.Unlock()
		if gc, ge := spec.LoadDirectoryConfig(d.configPath); ge == nil {
			for k, v := range gc.Defaults {
				global[k] = v
			}
			for k, v := range gc.Sections["global"] {
				global[k] = v
			}
			if n, e := strconv.Atoi(global["concurrency"]); e == nil && n > 0 {
				d.coord.SetConcurrency(n)
			}
			if n, e := strconv.Atoi(global["history"]); e == nil && n > 0 {
				d.store.SetHistoryLimit(n)
			}
			if v, ok := global["inherit_env"]; ok {
				d.stateMu.Lock()
				d.inheritEnv = v == "true"
				d.stateMu.Unlock()
				d.scheduler.SetInheritEnv(v == "true")
			}
			if dc := gc.Sections["daemon"]; dc != nil {
				d.stateMu.Lock()
				d.background = dc["background"] == "true"
				d.stateMu.Unlock()
				idleExit := time.Duration(0)
				if v := dc["idle_exit"]; v == "" || v == "0" {
					idleExit = 0
				} else if parsed, pe := time.ParseDuration(v); pe == nil {
					idleExit = parsed
				}
				d.stateMu.Lock()
				d.idleExit = idleExit
				d.stateMu.Unlock()
			}
			if pc := gc.Sections["persist"]; pc != nil {
				if v := pc["prune_interval"]; v != "" {
					if parsed, pe := time.ParseDuration(v); pe == nil {
						d.stateMu.Lock()
						d.pruneInterval = parsed
						d.stateMu.Unlock()
					}
				}
			}
			if rc := gc.Sections["persist.retention"]; rc != nil {
				if v := rc["runs_max"]; v != "" {
					d.stateMu.Lock()
					d.retention.RunsMax, _ = strconv.Atoi(v)
					d.stateMu.Unlock()
				}
				if v := rc["output_min"]; v != "" {
					d.stateMu.Lock()
					d.retention.OutputMin, _ = strconv.Atoi(v)
					d.stateMu.Unlock()
				}
				if v := rc["runs"]; v != "" {
					d.stateMu.Lock()
					d.retention.RunsAge, _ = time.ParseDuration(v)
					d.stateMu.Unlock()
				}
				if v := rc["output"]; v != "" {
					d.stateMu.Lock()
					d.retention.OutputAge, _ = time.ParseDuration(v)
					d.stateMu.Unlock()
				}
				if v := rc["transitions"]; v != "" {
					d.stateMu.Lock()
					d.retention.TransitionsAge, _ = time.ParseDuration(v)
					d.stateMu.Unlock()
				}
				if v := rc["events"]; v != "" {
					d.stateMu.Lock()
					d.retention.EventsAge, _ = time.ParseDuration(v)
					d.stateMu.Unlock()
				}
			}
		} else {
			globalErr = ge
			global = nil
		}
		d.stateMu.RLock()
		newIdleExit := d.idleExit
		d.stateMu.RUnlock()
		if oldIdleExit <= 0 && newIdleExit > 0 {
			d.startIdleMonitor()
		}
		d.stateMu.RLock()
		newPruneInterval := d.pruneInterval
		d.stateMu.RUnlock()
		if oldPruneInterval <= 0 && newPruneInterval > 0 {
			d.startPruneLoop()
		}
		select {
		case d.pruneWake <- struct{}{}:
		default:
		}
		seen := map[string]bool{}
		current := map[string]string{}
		d.stateMu.Lock()
		d.warnings = warnings
		d.stateMu.Unlock()
		for _, x := range f {
			m, he := spec.ParseHeader(x.Path)
			if he != nil {
				s := spec.Default(x)
				s.InvalidReason = he.Error()
				d.coord.Add(s)
				_ = persist.UpsertCheck(context.Background(), d.db, s, x.Group)
				seen[x.ID] = true
				current[x.ID] = rescanFingerprint(s)
				continue
			}
			layers, ce := spec.ChainWithHeader(d.Root, x.Path, x.ID, m)
			if ce != nil {
				s := spec.Default(x)
				s.InvalidReason = ce.Error()
				d.coord.Add(s)
				_ = persist.UpsertCheck(context.Background(), d.db, s, x.Group)
				seen[x.ID] = true
				current[x.ID] = rescanFingerprint(s)
				continue
			}
			if globalErr != nil {
				s := spec.Default(x)
				s.InvalidReason = globalErr.Error()
				d.coord.Add(s)
				_ = persist.UpsertCheck(context.Background(), d.db, s, x.Group)
				seen[x.ID] = true
				current[x.ID] = rescanFingerprint(s)
				continue
			}
			s, e := spec.Resolve(x, layers, global, spec.Overrides{AllowUnsafe: d.discoverOptions.AllowUnsafe})
			if e == nil {
				d.coord.Add(s)
				_ = persist.UpsertCheck(context.Background(), d.db, s, x.Group)
				seen[x.ID] = true
				current[x.ID] = rescanFingerprint(s)
			} else {
				s = spec.Default(x)
				s.InvalidReason = e.Error()
				d.coord.Add(s)
				_ = persist.UpsertCheck(context.Background(), d.db, s, x.Group)
				seen[x.ID] = true
				current[x.ID] = rescanFingerprint(s)
			}
		}
		removed := make([]string, 0)
		for _, x := range d.store.Snapshot() {
			if !seen[x.Spec.ID] {
				removed = append(removed, x.Spec.ID)
				if x.Running {
					d.stateMu.Lock()
					d.deletedPending[x.Spec.ID] = true
					d.stateMu.Unlock()
				} else {
					_ = persist.MarkDeleted(context.Background(), d.db, []string{x.Spec.ID})
					d.store.Remove(x.Spec.ID)
				}
			}
		}
		added := make([]string, 0)
		updated := make([]string, 0)
		for id, hash := range current {
			old, existed := before[id]
			if !existed {
				added = append(added, id)
			} else if old != hash {
				updated = append(updated, id)
			}
		}
		d.fingerprints = current
		sort.Strings(added)
		sort.Strings(removed)
		sort.Strings(updated)
		addedSpecs := make([]spec.Spec, 0, len(added))
		for _, id := range added {
			if entry, ok := d.store.Get(id); ok {
				addedSpecs = append(addedSpecs, entry.Spec)
			}
		}
		if len(addedSpecs) > 0 {
			for _, s := range addedSpecs {
				d.coord.PublishSpec("added", s.ID, s)
			}
			d.scheduler.Add(addedSpecs)
		}
		_ = persist.RecordEvent(context.Background(), d.db, "rescan", fmt.Sprintf("checks=%d", len(seen)))
		for _, id := range updated {
			if entry, ok := d.store.Get(id); ok && entry.Latest != nil {
				d.coord.PublishState("changed", id)
			}
		}
		d.stateMu.RLock()
		newBackground := d.background
		d.stateMu.RUnlock()
		shouldRun := newBackground || d.subscribers.Load() > 0
		if wasRunning && !shouldRun {
			d.scheduler.Stop()
		} else if wasRunning || oldBackground != newBackground || shouldRun {
			if wasRunning {
				d.scheduler.Stop()
			}
			d.scheduler.Start(context.Background(), d.specs())
		}
		return []rpc.Response{{ID: q.ID, Result: map[string]any{"added": added, "removed": removed, "updated": updated}, Final: true}}
	case "run":
		var p struct {
			IDs        []string      `json:"ids"`
			Filter     requestFilter `json:"filter"`
			Tag        string        `json:"tag"`
			Group      string        `json:"group"`
			Match      string        `json:"match"`
			Force      bool          `json:"force"`
			MaxAge     time.Duration `json:"max_age"`
			InheritEnv bool          `json:"inherit_env"`
			Wait       *bool         `json:"wait"`
			Timeout    time.Duration `json:"timeout"`
			Interval   time.Duration `json:"interval"`
		}
		_ = json.Unmarshal(q.Params, &p)
		entries, e := d.filteredEntries(q.Params)
		if e != nil {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "invalid_filter", Message: e.Error()}, Final: true}}
		}
		wait := true
		if p.Wait != nil {
			wait = *p.Wait
		}
		if !wait {
			ids := make([]string, 0)
			for _, e := range entries {
				ids = append(ids, e.Spec.ID)
				go func(id string) {
					ctx, done := d.beginRequest()
					defer done()
					_, _ = d.coord.Request(ctx, id, coordinator.Freshness{Force: p.Force, MaxAge: p.MaxAge, InheritEnv: p.InheritEnv, Timeout: p.Timeout, Interval: p.Interval}, "request")
				}(e.Spec.ID)
			}
			return []rpc.Response{{ID: q.ID, Result: map[string]any{"ids": ids}, Final: true}}
		}
		var outMu sync.Mutex
		var out []any
		var wg sync.WaitGroup
		for _, e := range entries {
			id := e.Spec.ID
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, done := d.beginRequest()
				defer done()
				v, e := d.coord.Request(ctx, id, coordinator.Freshness{Force: p.Force, MaxAge: p.MaxAge, InheritEnv: p.InheritEnv, Timeout: p.Timeout, Interval: p.Interval}, "request")
				if e == nil {
					outMu.Lock()
					out = append(out, v)
					outMu.Unlock()
				}
			}()
		}
		wg.Wait()
		return []rpc.Response{{ID: q.ID, Result: out, Final: true}}
	case "history":
		var p struct {
			CheckID string    `json:"check_id"`
			Limit   int       `json:"limit"`
			Since   time.Time `json:"since"`
			Until   time.Time `json:"until"`
		}
		_ = json.Unmarshal(q.Params, &p)
		rows, e := persist.HistoryBetween(context.Background(), d.db, p.CheckID, p.Limit, p.Since, p.Until)
		if e != nil {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "history_error", Message: e.Error()}, Final: true}}
		}
		return []rpc.Response{{ID: q.ID, Result: rows, Final: true}}
	case "transitions":
		var p struct {
			CheckID string    `json:"check_id"`
			Since   time.Time `json:"since"`
			Until   time.Time `json:"until"`
		}
		_ = json.Unmarshal(q.Params, &p)
		if p.Until.IsZero() {
			p.Until = time.Now()
		}
		rows, e := persist.Transitions(context.Background(), d.db, p.CheckID, p.Since, p.Until)
		if e != nil {
			return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "transitions_error", Message: e.Error()}, Final: true}}
		}
		return []rpc.Response{{ID: q.ID, Result: rows, Final: true}}
	case "shutdown":
		var p struct {
			Drain *bool `json:"drain"`
		}
		_ = json.Unmarshal(q.Params, &p)
		drain := true
		if p.Drain != nil {
			drain = *p.Drain
		}
		go d.shutdown(drain)
		return []rpc.Response{{ID: q.ID, Result: map[string]bool{"stopping": true}, Final: true}}
	default:
		return []rpc.Response{{ID: q.ID, Error: &rpc.RPCError{Code: "unknown_method", Message: q.Method}, Final: true}}
	}
}
func (d *Daemon) idleMonitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case now := <-t.C:
			d.stateMu.RLock()
			idleExit := d.idleExit
			d.stateMu.RUnlock()
			if idleExit <= 0 {
				continue
			}
			busy := false
			for _, e := range d.store.Snapshot() {
				if e.Running {
					busy = true
					break
				}
			}
			if !busy && now.Sub(time.Unix(0, d.lastActivity.Load())) >= idleExit {
				d.Stop()
				return
			}
		}
	}
}
func (d *Daemon) startIdleMonitor() {
	if d.idleStarted.CompareAndSwap(false, true) {
		go d.idleMonitor()
	}
}
func (d *Daemon) rootMonitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var missing time.Time
	for {
		select {
		case <-d.stop:
			return
		case now := <-t.C:
			if _, e := os.Stat(d.Root); e != nil {
				if missing.IsZero() {
					missing = now
				}
				if now.Sub(missing) >= time.Minute {
					d.Stop()
					return
				}
			} else {
				missing = time.Time{}
			}
		}
	}
}
func (d *Daemon) pruneLoop() {
	for {
		d.stateMu.RLock()
		interval := d.pruneInterval
		retention := d.retention
		d.stateMu.RUnlock()
		if interval <= 0 {
			select {
			case <-d.stop:
				return
			case <-d.pruneWake:
				continue
			}
		}
		t := time.NewTimer(interval)
		select {
		case <-d.stop:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			return
		case <-d.pruneWake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			continue
		case <-t.C:
			if e := persist.PruneWithOptions(context.Background(), d.db, retention); e != nil {
				_ = persist.RecordEvent(context.Background(), d.db, "db_error", e.Error())
			} else {
				_ = persist.RecordEvent(context.Background(), d.db, "prune", "periodic")
			}
		}
	}
}
func (d *Daemon) startPruneLoop() {
	if d.pruneStarted.CompareAndSwap(false, true) {
		go d.pruneLoop()
	}
}

func (d *Daemon) rescanLoop() {
	t := time.NewTicker(d.rescanInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			_ = d.Reload()
		}
	}
}
func (d *Daemon) specs() []spec.Spec {
	var out []spec.Spec
	for _, e := range d.store.Snapshot() {
		out = append(out, e.Spec)
	}
	return out
}

func rescanFingerprint(s spec.Spec) string {
	stamp := int64(0)
	size := int64(0)
	contentHash := ""
	if st, err := os.Stat(s.Path); err == nil {
		stamp = st.ModTime().UnixNano()
		size = st.Size()
		if f, err := os.Open(s.Path); err == nil {
			h := sha256.New()
			_, _ = io.Copy(h, f)
			_ = f.Close()
			contentHash = hex.EncodeToString(h.Sum(nil))
		}
	}
	return fmt.Sprintf("%s:%d:%d:%s", s.Hash, stamp, size, contentHash)
}
func (d *Daemon) stateDir(id string) string {
	if d.noState {
		return ""
	}
	if d.stateRoot != "" {
		return filepath.Join(d.stateRoot, strings.ReplaceAll(id, "/", "%2F"))
	}
	return filepath.Join(filepath.Dir(d.DBPath), "state", strings.ReplaceAll(id, "/", "%2F"))
}
func latestStatus(e coordinator.Entry) string {
	if e.Latest == nil {
		return "PENDING"
	}
	return string(e.Latest.Status)
}
func (d *Daemon) Stop() {
	d.once.Do(func() {
		// A normal process stop (SIGTERM/SIGINT and embedded callers) drains
		// active client requests. The explicit shutdown{drain:false} path
		// performs cancellation before reaching Stop.
		d.stopRequests(true)
		if d.scheduler != nil {
			d.scheduler.Stop()
		}
		close(d.stop)
		if d.ln != nil {
			_ = d.ln.Close()
		}
		_ = persist.RecordEvent(context.Background(), d.db, "stop", "shutdown")
		d.writeLog("INFO", "daemon stopped", nil)
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.writer.Close(flushCtx)
		cancel()
		if d.db != nil {
			_ = d.db.Close()
		}
		_ = os.Remove(d.Socket)
		if d.lock != nil {
			_ = syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
			_ = d.lock.Close()
		}
		d.closeLog()
	})
}

// ServeConn serves one already-established RPC connection. It is useful for
// embedded callers and benchmarks that need the exact daemon protocol without
// opening another Unix socket.
func (d *Daemon) ServeConn(conn net.Conn) {
	rpc.ServeStream(conn, d.handle, d.stream)
}

// SetConcurrency changes the worker limit for an embedded daemon before it is
// serving requests. It is intentionally small so benchmark and embedding
// callers use the same coordinator as the production daemon.
func (d *Daemon) SetConcurrency(n int) {
	if d.coord != nil {
		d.coord.SetConcurrency(n)
	}
}

func writeLockInfo(f *os.File, started time.Time) error {
	if _, e := f.Seek(0, 0); e != nil {
		return e
	}
	if e := f.Truncate(0); e != nil {
		return e
	}
	b, e := json.Marshal(map[string]any{"pid": os.Getpid(), "version": version.Current, "started_at": started, "mode": "daemon"})
	if e != nil {
		return e
	}
	b = append(b, '\n')
	_, e = f.Write(b)
	return e
}

const maxLogBytes = 10 << 20

func (d *Daemon) openLog() error {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	f, e := os.OpenFile(d.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	_ = os.Chmod(d.logPath, 0600)
	d.logFile = f
	return nil
}

func (d *Daemon) closeLog() {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	if d.logFile != nil {
		_ = d.logFile.Close()
		d.logFile = nil
	}
}

func (d *Daemon) rotateLogLocked(next int) {
	if d.logFile != nil {
		_ = d.logFile.Close()
		d.logFile = nil
	}
	for i := 3; i >= 1; i-- {
		src := d.logPath
		if i > 1 {
			src += fmt.Sprintf(".%d", i-1)
		}
		dst := d.logPath + fmt.Sprintf(".%d", i)
		if i == 3 {
			_ = os.Remove(dst)
		}
		_ = os.Rename(src, dst)
	}
	f, e := os.OpenFile(d.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e == nil {
		_ = os.Chmod(d.logPath, 0600)
		d.logFile = f
	}
}

func (d *Daemon) writeLog(level, message string, fields map[string]any) {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	if d.logFile == nil {
		return
	}
	entry := map[string]any{"time": time.Now().UTC(), "level": level, "msg": message}
	for k, v := range fields {
		entry[k] = v
	}
	b, e := json.Marshal(entry)
	if e != nil {
		return
	}
	b = append(b, '\n')
	if st, se := d.logFile.Stat(); se == nil && st.Size()+int64(len(b)) > maxLogBytes {
		d.rotateLogLocked(len(b))
	}
	if d.logFile != nil {
		_, _ = d.logFile.Write(b)
	}
}

func (d *Daemon) beginRequest() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	d.activeMu.Lock()
	d.activeNext++
	id := d.activeNext
	d.active[id] = cancel
	d.activeMu.Unlock()
	return ctx, func() {
		d.activeMu.Lock()
		delete(d.active, id)
		d.activeMu.Unlock()
		cancel()
	}
}

func (d *Daemon) stopRequests(drain bool) {
	if !drain {
		d.activeMu.Lock()
		for _, cancel := range d.active {
			cancel()
		}
		d.activeMu.Unlock()
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			d.activeMu.Lock()
			n := len(d.active)
			d.activeMu.Unlock()
			if n == 0 {
				return
			}
			select {
			case <-deadline.C:
				return
			case <-ticker.C:
			}
		}
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		d.activeMu.Lock()
		n := len(d.active)
		d.activeMu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func (d *Daemon) shutdown(drain bool) {
	d.stopRequests(drain)
	d.Stop()
}

// Reload applies the same discovery/configuration path as the reload RPC.
// It is provided for signal handlers and embedding callers.
func (d *Daemon) Reload() error {
	res := d.handle(rpc.Request{ID: 0, Method: "reload"})
	if len(res) == 0 || res[0].Error != nil {
		if len(res) > 0 && res[0].Error != nil {
			return fmt.Errorf("reload: %s", res[0].Error.Message)
		}
		return fmt.Errorf("reload failed")
	}
	return nil
}
