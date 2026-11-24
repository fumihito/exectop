package bench

import (
	"context"
	"encoding/json"
	"exectop/src/coordinator"
	"exectop/src/daemon"
	"exectop/src/discover"
	"exectop/src/metrics"
	"exectop/src/persist"
	"exectop/src/rpc"
	"exectop/src/runner"
	"exectop/src/sched"
	"exectop/src/spec"
	"exectop/src/tui"
	"exectop/src/version"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type GenOptions struct {
	Out              string
	Checks           int
	Mean             time.Duration
	FailRatio        float64
	OutputBytes      int
	Seed             int64
	Dist             string
	MinIntervalRatio float64
	Kind             string
	Helper           string
	IntervalMin      time.Duration
	IntervalMax      time.Duration
}

func Generate(o GenOptions) error {
	if o.Checks <= 0 {
		o.Checks = 500
	}
	if o.Mean < 0 {
		o.Mean = 200 * time.Millisecond
	}
	if o.IntervalMin < 0 || o.IntervalMax < 0 || (o.IntervalMax > 0 && o.IntervalMin > o.IntervalMax) {
		return fmt.Errorf("invalid interval range")
	}
	if o.FailRatio < 0 {
		o.FailRatio = .1
	}
	if o.OutputBytes < 0 {
		o.OutputBytes = 256
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
	if e := os.MkdirAll(o.Out, 0700); e != nil {
		return e
	}
	r := rand.New(rand.NewSource(o.Seed))
	if o.Dist == "" {
		o.Dist = "lognormal"
	}
	if o.Kind == "" {
		o.Kind = "sh"
	}
	configs := map[string][]string{}
	groups := int(math.Sqrt(float64(o.Checks)))
	if groups < 1 {
		groups = 1
	}
	for i := 0; i < o.Checks; i++ {
		g := filepath.Join(o.Out, fmt.Sprintf("g%03d", i%groups))
		if e := os.MkdirAll(g, 0700); e != nil {
			return e
		}
		d := o.Mean
		switch o.Dist {
		case "fixed":
		case "uniform":
			d = time.Duration(float64(d) * (0.5 + r.Float64()))
		case "bimodal":
			if r.Float64() >= .9 {
				d *= 8
			} else {
				d = time.Duration(float64(d) * .5)
			}
		default: // lognormal-like bounded approximation, deterministic and portable
			d = time.Duration(float64(d) * (0.5 + r.Float64()))
		}
		fail := r.Float64() < o.FailRatio
		code := 0
		if fail {
			code = 1
		}
		p := filepath.Join(g, fmt.Sprintf("check-%05d.sh", i))
		interval := o.Mean + time.Second
		if o.IntervalMin > 0 || o.IntervalMax > 0 {
			lo, hi := o.IntervalMin, o.IntervalMax
			if hi == 0 {
				hi = lo
			}
			if hi == lo {
				interval = lo
			} else {
				interval = lo + time.Duration(r.Int63n(int64(hi-lo)+1))
			}
		}
		min := time.Duration(0)
		if o.MinIntervalRatio > 0 && r.Float64() < o.MinIntervalRatio {
			min = o.Mean * 20
		}
		timeout := o.Mean * 10
		if timeout < time.Second {
			// Very short checks still pay shell/process startup overhead. A
			// zero mean is also used by the overhead benchmark, and must not
			// become a zero runner timeout before its helper can start.
			timeout = time.Second
		}
		kind := "#!/bin/sh\n"
		if o.Kind == "bin" && o.Helper != "" {
			p = filepath.Join(g, fmt.Sprintf("benchcheck-%05d-%d-%d-%d", i, d.Nanoseconds(), o.OutputBytes, code))
			if e := os.Link(o.Helper, p); e != nil {
				data, re := os.ReadFile(o.Helper)
				if re != nil {
					return re
				}
				if e = os.WriteFile(p, data, 0700); e != nil {
					return e
				}
			}
			configs[g] = append(configs[g], fmt.Sprintf("[checks.\"%s\"]\ninterval=\"%s\"\ntimeout=\"%s\"\nmin_interval=\"%s\"\n", filepath.Base(p), interval, timeout, min))
			continue
		}
		// Go durations (for example, 1ms) are not valid operands for the
		// POSIX sleep utility. Emit fractional seconds so generated checks
		// actually exercise the requested workload instead of silently
		// continuing after a sleep error.
		shellDelay := strconv.FormatFloat(d.Seconds(), 'f', 9, 64)
		body := fmt.Sprintf("%s# exectop: interval=%s timeout=%s min_interval=%s\nhead -c %d /dev/zero | tr '\\000' x\nprintf '\\n'\nsleep %s\nexit %d\n", kind, interval, timeout, min, o.OutputBytes, shellDelay, code)
		if e := os.WriteFile(p, []byte(body), 0700); e != nil {
			return e
		}
	}
	for dir, sections := range configs {
		if e := os.WriteFile(filepath.Join(dir, ".exectop.toml"), []byte(strings.Join(sections, "\n")), 0600); e != nil {
			return e
		}
	}
	return nil
}
func ParseBytes(s string) (int, error) {
	if len(s) == 0 {
		return 0, nil
	}
	mul := 1
	for suffix, factor := range map[string]int{"GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024, "Gi": 1024 * 1024 * 1024, "Mi": 1024 * 1024, "Ki": 1024, "G": 1024 * 1024 * 1024, "M": 1024 * 1024, "K": 1024} {
		if len(s) > len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
			mul = factor
			s = s[:len(s)-len(suffix)]
			break
		}
	}
	n, e := strconv.Atoi(s)
	return n * mul, e
}

func DiscoverMetric(root string) (map[string]any, error) {
	start := time.Now()
	f, _, e := discover.Discover(root, discover.Options{})
	if e != nil {
		return nil, e
	}
	mid := time.Now()
	for _, x := range f {
		if _, e = spec.ParseHeader(x.Path); e != nil {
			return nil, e
		}
	}
	return map[string]any{"discover_ms": mid.Sub(start).Milliseconds(), "spec_ms": time.Since(mid).Milliseconds(), "checks": len(f)}, nil
}
func Save(v any, path string) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, b, 0600)
}

type RunOptions struct {
	Root          string
	Concurrency   int
	Scenarios     []string
	NoDaemon      bool
	Repeat        int
	Warmup        int
	WatchDuration time.Duration
	Kind          string
	Mean          time.Duration
	Dist          string
	FailRatio     float64
}
type RunReport struct {
	ExectopVersion string                    `json:"exectop_version"`
	Host           map[string]any            `json:"host"`
	Params         map[string]any            `json:"params"`
	Root           string                    `json:"root"`
	Checks         int                       `json:"checks"`
	Scenarios      map[string]map[string]any `json:"scenarios"`
	Pass           bool                      `json:"pass"`
	Failures       []string                  `json:"failures"`
}

func Run(o RunOptions) (RunReport, error) {
	if o.Repeat < 1 {
		o.Repeat = 1
	}
	if o.Warmup < 0 {
		o.Warmup = 0
	}
	if o.WatchDuration <= 0 {
		o.WatchDuration = time.Minute
	}
	for i := 0; i < o.Warmup; i++ {
		if _, e := runOnce(o); e != nil {
			return RunReport{}, e
		}
	}
	if o.Repeat == 1 {
		return runOnce(o)
	}
	reports := make([]RunReport, 0, o.Repeat)
	for i := 0; i < o.Repeat; i++ {
		x, e := runOnce(o)
		if e != nil {
			return RunReport{}, e
		}
		reports = append(reports, x)
	}
	out := reports[0]
	// Individual runs may contain transient performance-gate failures. The
	// repeated report is judged from its medians, so do not carry the first
	// run's failure list into the aggregate.
	out.Pass = true
	out.Failures = nil
	for name := range out.Scenarios {
		all := make([]any, 0, len(reports))
		numeric := map[string][]float64{}
		for _, x := range reports {
			v := x.Scenarios[name]
			// Do not retain the report's scenario map itself: the first report is
			// reused as the aggregate below, and adding "runs" to it would make
			// the JSON report recursively contain itself.
			copyOfScenario := make(map[string]any, len(v))
			for k, value := range v {
				copyOfScenario[k] = value
			}
			all = append(all, copyOfScenario)
			for k, n := range v {
				if f, ok := numericValue(n); ok {
					numeric[k] = append(numeric[k], f)
				}
			}
		}
		median := map[string]any{}
		for k, vals := range numeric {
			sort.Float64s(vals)
			median[k] = vals[len(vals)/2]
		}
		out.Scenarios[name]["repeat"] = len(reports)
		out.Scenarios[name]["warmup"] = o.Warmup
		out.Scenarios[name]["runs"] = all
		out.Scenarios[name]["median"] = median
	}
	evaluateGates(&out)
	return out, nil
}
func runOnce(o RunOptions) (RunReport, error) {
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	f, _, e := discover.Discover(o.Root, discover.Options{})
	if e != nil {
		return RunReport{}, e
	}
	ss := make([]spec.Spec, 0, len(f))
	for _, x := range f {
		m, _ := spec.ParseHeader(x.Path)
		s, e := spec.Resolve(x, []map[string]string{m}, nil, spec.Overrides{})
		if e != nil {
			continue
		}
		ss = append(ss, s)
	}
	host, _ := os.Hostname()
	kind := o.Kind
	if kind == "" {
		kind = "sh"
	}
	r := RunReport{ExectopVersion: version.Current, Host: hostInfo(host, o.Root), Params: map[string]any{"checks": len(ss), "concurrency": o.Concurrency, "kind": kind, "no_daemon": o.NoDaemon, "mean_ms": o.Mean.Milliseconds(), "dist": o.Dist, "fail_ratio": o.FailRatio, "repeat": o.Repeat, "warmup": o.Warmup, "watch_duration_ms": o.WatchDuration.Milliseconds()}, Root: o.Root, Checks: len(ss), Scenarios: map[string]map[string]any{}, Pass: true}
	sc := o.Scenarios
	if len(sc) == 0 {
		sc = []string{"discover", "run-cold", "run-cached", "run-standalone", "overhead", "join-storm", "watch-steady", "rpc-latency", "persist-burst", "persist-slowdisk", "restore", "prune", "tui-render"}
	}
	if len(sc) == 1 && sc[0] == "all" {
		sc = []string{"discover", "run-cold", "run-cached", "run-standalone", "overhead", "join-storm", "watch-steady", "rpc-latency", "persist-burst", "persist-slowdisk", "restore", "prune", "tui-render"}
	}
	for _, name := range sc {
		switch name {
		case "discover":
			m, e := DiscoverMetric(o.Root)
			if e != nil {
				return r, e
			}
			r.Scenarios[name] = m
		case "run-cold":
			start := time.Now()
			var rs []runner.Result
			if o.NoDaemon {
				rs = execute(ss, o.Concurrency)
			} else {
				rs, e = executeDaemonRPC(o.Root, ss, o.Concurrency)
				if e != nil {
					return r, e
				}
			}
			wall := time.Since(start)
			sum := time.Duration(0)
			for _, x := range rs {
				sum += x.Duration
			}
			eff := 0.0
			if wall > 0 {
				eff = float64(sum) / float64(wall*time.Duration(o.Concurrency))
			}
			r.Scenarios[name] = map[string]any{"wall_ms": wall.Milliseconds(), "sum_ms": sum.Milliseconds(), "parallel_efficiency": eff, "executed": len(rs)}
		case "overhead":
			if o.Kind != "bin" || o.Mean != 0 {
				r.Scenarios[name] = map[string]any{"skipped": true, "reason": "requires kind=bin and mean=0"}
				continue
			}
			start := time.Now()
			rs := execute(ss, o.Concurrency)
			wall := time.Since(start)
			overhead := time.Duration(0)
			if o.Kind == "bin" {
				expectedTotal := time.Duration(0)
				for _, s := range ss {
					expectedTotal += benchCheckDuration(s.Path)
				}
				overhead = wall*time.Duration(o.Concurrency) - expectedTotal
				if overhead < 0 {
					overhead = 0
				}
			} else if len(ss) > 0 {
				overhead = wall * time.Duration(o.Concurrency) / time.Duration(len(ss))
			}
			perCheck := float64(0)
			if len(rs) > 0 {
				perCheck = float64(overhead.Microseconds()) / float64(len(rs))
			}
			r.Scenarios[name] = map[string]any{"wall_ms": wall.Milliseconds(), "overhead_us": perCheck}
		case "run-cached":
			var start time.Time
			var cold, cached []runner.Result
			if o.NoDaemon {
				c := coordinator.New(nil, func(ctx context.Context, s spec.Spec, m runner.Meta) runner.Result { return runner.Run(ctx, s, m) })
				c.SetConcurrency(o.Concurrency)
				for _, s := range ss {
					s.MinInterval = time.Hour
					s.MinIntervalFail = time.Hour
					c.Add(s)
					v, _ := c.Request(context.Background(), s.ID, coordinator.Freshness{Force: true}, "request")
					cold = append(cold, v.Result)
				}
				start = time.Now()
				for _, s := range ss {
					v, _ := c.Request(context.Background(), s.ID, coordinator.Freshness{}, "request")
					cached = append(cached, v.Result)
				}
			} else {
				d, de := daemon.New(o.Root)
				if de != nil {
					return r, de
				}
				d.SetConcurrency(o.Concurrency)
				cold, de = executeDaemonRPCRequest(d, ss, true)
				if de == nil {
					start = time.Now()
					cached, de = executeDaemonRPCRequest(d, ss, false)
				}
				d.Stop()
				if de != nil {
					return r, de
				}
			}
			cachedCount := 0
			for _, v := range cached {
				if v.Cached {
					cachedCount++
				}
			}
			r.Scenarios[name] = map[string]any{"wall_ms": time.Since(start).Milliseconds(), "cached_count": cachedCount, "all_cached": cachedCount == len(ss), "executed": len(cold), "rpc_run_us": float64(time.Since(start).Microseconds()) / float64(max(1, len(ss)))}
		case "run-standalone":
			start := time.Now()
			rs := execute(ss, o.Concurrency)
			r.Scenarios[name] = map[string]any{"wall_ms": time.Since(start).Milliseconds(), "executed": len(rs), "mode": "standalone"}
		case "join-storm":
			if len(ss) == 0 {
				r.Scenarios[name] = map[string]any{"clients": 0, "executions": 0}
				break
			}
			var executions atomic.Int64
			c := coordinator.New(nil, func(ctx context.Context, s spec.Spec, m runner.Meta) runner.Result {
				executions.Add(1)
				return runner.Run(ctx, s, m)
			})
			c.Add(ss[0])
			start := time.Now()
			var wg sync.WaitGroup
			for i := 0; i < 50; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = c.Request(context.Background(), ss[0].ID, coordinator.Freshness{Force: true}, "request")
				}()
			}
			wg.Wait()
			r.Scenarios[name] = map[string]any{"clients": 50, "executions": executions.Load(), "join_latency_ms": time.Since(start).Milliseconds()}
		case "watch-steady", "persist-slowdisk":
			if name == "watch-steady" {
				c := coordinator.New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
					now := time.Now()
					return runner.Result{ID: s.ID, Status: "OK", Start: now, End: now}
				})
				for _, s := range ss {
					c.Add(s)
				}
				sc := sched.New(c)
				before := runtime.NumGoroutine()
				metrics.Reset("sched.lag")
				start := time.Now()
				sc.Start(context.Background(), ss)
				time.Sleep(o.WatchDuration)
				sc.Stop()
				elapsed := time.Since(start)
				overruns := 0
				for _, n := range sc.Overruns {
					overruns += n
				}
				lagP99 := metricNumber("sched.lag", "p99_us") / 1000
				goroutines := runtime.NumGoroutine() - before
				if goroutines < 0 {
					goroutines = 0
				}
				r.Scenarios[name] = map[string]any{"duration_s": elapsed.Seconds(), "overruns": overruns, "sched_lag_ms_p99": lagP99, "goroutines": goroutines, "rss_mb": processRSSMB(), "dropped": 0}
			} else {
				dir, de := os.MkdirTemp("", "exectop-slowdisk-")
				if de != nil {
					return r, de
				}
				db, de := persist.Open(filepath.Join(dir, "slow.db"))
				if de != nil {
					_ = os.RemoveAll(dir)
					return r, de
				}
				for _, s := range ss {
					_ = persist.UpsertCheck(context.Background(), db, s, "")
				}
				metrics.Reset("persist.commit")
				delay := 50 * time.Millisecond
				if v := os.Getenv("EXECTOP_BENCH_COMMIT_DELAY"); v != "" {
					if parsed, pe := time.ParseDuration(v); pe == nil && parsed >= 0 {
						delay = parsed
					}
				}
				w := persist.NewWriterWithConfig(db, persist.WriterConfig{QueueSize: max(1, len(ss)*2), BatchSize: 1, BatchInterval: time.Millisecond, CommitDelay: delay})
				c := coordinator.New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
					now := time.Now()
					return runner.Result{ID: s.ID, Status: "OK", Start: now, End: now}
				})
				c.SetConcurrency(o.Concurrency)
				c.SetResultHook(func(r runner.Result) { w.Enqueue(persist.Item{Result: r}) })
				sc := sched.New(c)
				before := runtime.NumGoroutine()
				metrics.Reset("sched.lag")
				start := time.Now()
				sc.Start(context.Background(), ss)
				time.Sleep(o.WatchDuration)
				sc.Stop()
				elapsed := time.Since(start)
				stats := w.Stats()
				lag := metricNumber("sched.lag", "p99_us") / 1000
				goroutines := runtime.NumGoroutine() - before
				if goroutines < 0 {
					goroutines = 0
				}
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = w.Close(closeCtx)
				closeCancel()
				db.Close()
				_ = os.RemoveAll(dir)
				commit := metrics.SnapshotAll()
				r.Scenarios[name] = map[string]any{"duration_s": elapsed.Seconds(), "overruns": sumOverruns(sc.OverrunSnapshot()), "sched_lag_ms_p99": lag, "goroutines": goroutines, "commit_delay_ms": delay.Milliseconds(), "commit_ms_p50": metricNumberFrom(commit, "persist.commit", "p50_us") / 1000, "commit_ms_p99": metricNumberFrom(commit, "persist.commit", "p99_us") / 1000, "dropped": stats.Dropped}
			}
		case "rpc-latency":
			hello, run, e := rpcLatency(o, ss)
			if e != nil {
				return r, e
			}
			r.Scenarios[name] = map[string]any{"iterations": 1000, "hello_p50_us": percentile(hello, .5), "hello_p99_us": percentile(hello, .99), "run_wait_false_p50_us": percentile(run, .5), "run_wait_false_p99_us": percentile(run, .99)}
		case "persist-burst":
			dir, de := os.MkdirTemp("", "exectop-bench-")
			if de != nil {
				return r, de
			}
			db, de := persist.Open(filepath.Join(dir, "bench.db"))
			if de != nil {
				_ = os.RemoveAll(dir)
				return r, de
			}
			for _, s := range ss {
				_ = persist.UpsertCheck(context.Background(), db, s, "")
			}
			metrics.Reset("persist.commit")
			w := persist.NewWriterWithConfig(db, persist.WriterConfig{QueueSize: 10000, BatchSize: 200, BatchInterval: time.Millisecond})
			start := time.Now()
			count := 10000
			for i := 0; i < count && len(ss) > 0; i++ {
				s := ss[i%len(ss)]
				now := time.Now()
				w.Enqueue(persist.Item{Result: runner.Result{ID: s.ID, Iter: i + 1, Status: "OK", Start: now, End: now, SpecHash: s.Hash}})
			}
			if de = w.Close(context.Background()); de != nil {
				db.Close()
				_ = os.RemoveAll(dir)
				return r, de
			}
			elapsed := time.Since(start)
			stats := w.Stats()
			db.Close()
			_ = os.RemoveAll(dir)
			commit := metrics.SnapshotAll()
			r.Scenarios[name] = map[string]any{"count": count, "commit_ms_p50": metricNumberFrom(commit, "persist.commit", "p50_us") / 1000, "commit_ms_p99": metricNumberFrom(commit, "persist.commit", "p99_us") / 1000, "elapsed_ms": elapsed.Milliseconds(), "queue_depth_max": stats.QueueDepthMax, "dropped": stats.Dropped}
		case "restore":
			dir, de := os.MkdirTemp("", "exectop-restore-")
			if de != nil {
				return r, de
			}
			db, de := persist.Open(filepath.Join(dir, "restore.db"))
			if de != nil {
				_ = os.RemoveAll(dir)
				return r, de
			}
			for _, s := range ss {
				_ = persist.UpsertCheck(context.Background(), db, s, "")
			}
			w := persist.NewWriter(db, max(1, len(ss)*2))
			for i, s := range ss {
				now := time.Now()
				w.Enqueue(persist.Item{Result: runner.Result{ID: s.ID, Iter: i + 1, Status: "OK", Start: now, End: now, SpecHash: s.Hash}})
			}
			_ = w.Close(context.Background())
			ids := make([]string, 0, len(ss))
			for _, s := range ss {
				ids = append(ids, s.ID)
			}
			start := time.Now()
			latestRows, de := persist.LoadLatest(context.Background(), db, ids)
			restoreMS := time.Since(start).Milliseconds()
			db.Close()
			_ = os.RemoveAll(dir)
			if de != nil {
				return r, de
			}
			r.Scenarios[name] = map[string]any{"restore_ms": restoreMS, "cached_count": len(latestRows), "rows": len(latestRows)}
		case "prune":
			dir, de := os.MkdirTemp("", "exectop-prune-")
			if de != nil {
				return r, de
			}
			db, de := persist.Open(filepath.Join(dir, "prune.db"))
			if de != nil {
				_ = os.RemoveAll(dir)
				return r, de
			}
			for _, s := range ss {
				_ = persist.UpsertCheck(context.Background(), db, s, "")
			}
			w := persist.NewWriter(db, max(1, len(ss)*2))
			for i := 0; i < max(1, len(ss))*3; i++ {
				if len(ss) > 0 {
					s := ss[i%len(ss)]
					now := time.Now()
					w.Enqueue(persist.Item{Result: runner.Result{ID: s.ID, Iter: i + 1, Status: "OK", Start: now, End: now, SpecHash: s.Hash}})
				}
			}
			_ = w.Close(context.Background())
			start := time.Now()
			de = persist.Prune(context.Background(), db, 2)
			pruneMS := time.Since(start).Milliseconds()
			var rows int
			if de == nil {
				_ = db.QueryRow("SELECT count(*) FROM runs").Scan(&rows)
			}
			db.Close()
			_ = os.RemoveAll(dir)
			if de != nil {
				return r, de
			}
			r.Scenarios[name] = map[string]any{"prune_ms": pruneMS, "rows": rows}
		case "tui-render":
			a := tui.NewApp(nil)
			for _, s := range ss {
				a.AddSpec(s)
			}
			durations := make([]time.Duration, 0, 1000)
			for i := 0; i < 1000; i++ {
				start := time.Now()
				_ = a.View()
				durations = append(durations, time.Since(start))
			}
			r.Scenarios[name] = map[string]any{"iterations": 1000, "render_ms_p99": float64(percentile(durations, .99)) / 1000}
		default:
			r.Failures = append(r.Failures, name)
			r.Pass = false
		}
	}
	evaluateGates(&r)
	return r, nil
}

func executeDaemonRPC(root string, ss []spec.Spec, concurrency int) ([]runner.Result, error) {
	d, err := daemon.New(root)
	if err != nil {
		return nil, err
	}
	d.SetConcurrency(concurrency)
	defer d.Stop()
	return executeDaemonRPCRequest(d, ss, true)
}

func executeDaemonRPCRequest(d *daemon.Daemon, ss []spec.Spec, force bool) ([]runner.Result, error) {
	var err error
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		d.ServeConn(server)
		close(done)
	}()
	defer func() {
		_ = client.Close()
		<-done
	}()
	reader := rpc.NewReader(client)
	write := func(id int64, method string, params any) error {
		b, e := json.Marshal(params)
		if e != nil {
			return e
		}
		return rpc.Write(client, rpc.Request{ID: id, Method: method, Params: b})
	}
	if err = write(0, "hello", map[string]any{"proto_version": 1}); err != nil {
		return nil, err
	}
	if _, err = reader.Read(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(ss))
	for _, s := range ss {
		ids = append(ids, s.ID)
	}
	if err = write(1, "run", map[string]any{"ids": ids, "force": force, "wait": true}); err != nil {
		return nil, err
	}
	results := make([]runner.Result, 0, len(ids))
	for {
		raw, re := reader.Read()
		if re != nil {
			return nil, re
		}
		var response rpc.Response
		if re = json.Unmarshal(raw, &response); re != nil {
			return nil, re
		}
		if response.Error != nil {
			return nil, fmt.Errorf("daemon run: %s", response.Error.Message)
		}
		if response.Result != nil {
			payload, me := json.Marshal(response.Result)
			if me != nil {
				return nil, me
			}
			var served struct {
				Result runner.Result `json:"result"`
			}
			if me = json.Unmarshal(payload, &served); me != nil {
				return nil, me
			}
			if served.Result.ID != "" {
				results = append(results, served.Result)
			}
		}
		if response.Final {
			return results, nil
		}
	}
}

func rpcLatency(o RunOptions, ss []spec.Spec) ([]time.Duration, []time.Duration, error) {
	var d *daemon.Daemon
	var err error
	server, client := net.Pipe()
	serveDone := make(chan struct{})
	if o.NoDaemon {
		go func() {
			rpc.Serve(server, func(q rpc.Request) []rpc.Response {
				return []rpc.Response{{ID: q.ID, Result: map[string]any{"ok": true}, Final: true}}
			})
			close(serveDone)
		}()
	} else {
		d, err = daemon.New(o.Root)
		if err != nil {
			_ = client.Close()
			_ = server.Close()
			return nil, nil, err
		}
		go func() {
			d.ServeConn(server)
			close(serveDone)
		}()
	}
	reader := rpc.NewReader(client)
	closeConnection := func() {
		_ = client.Close()
		if d != nil {
			d.Stop()
		}
		<-serveDone
	}
	defer closeConnection()
	request := func(id int64, method string, params any) error {
		b, e := json.Marshal(params)
		if e != nil {
			return e
		}
		if e = rpc.Write(client, rpc.Request{ID: id, Method: method, Params: b}); e != nil {
			return e
		}
		_, e = reader.Read()
		return e
	}
	if err = request(0, "hello", map[string]any{"proto_version": 1}); err != nil {
		return nil, nil, err
	}
	ids := []string{}
	if len(ss) > 0 {
		ids = []string{ss[0].ID}
	}
	hello := make([]time.Duration, 1000)
	run := make([]time.Duration, 1000)
	for i := 0; i < len(hello); i++ {
		start := time.Now()
		if err = request(int64(i+1), "hello", map[string]any{"proto_version": 1}); err != nil {
			return nil, nil, err
		}
		hello[i] = time.Since(start)
		start = time.Now()
		if err = request(int64(i+1), "run", map[string]any{"ids": ids, "wait": false}); err != nil {
			return nil, nil, err
		}
		run[i] = time.Since(start)
	}
	return hello, run, nil
}

func hostInfo(host, root string) map[string]any {
	info := map[string]any{"hostname": host, "ncpu": runtime.NumCPU(), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
	if b, e := os.ReadFile("/proc/cpuinfo"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.IndexByte(line, ':'); i >= 0 {
					info["cpu"] = strings.TrimSpace(line[i+1:])
				}
				break
			}
		}
	}
	if b, e := os.ReadFile("/proc/sys/kernel/osrelease"); e == nil {
		info["kernel"] = strings.TrimSpace(string(b))
	}
	var st syscall.Statfs_t
	if e := syscall.Statfs(root, &st); e == nil {
		fsType := fmt.Sprintf("0x%x", st.Type)
		info["fs"] = fsType
		info["fs_type"] = fsType
	}
	return info
}

func evaluateGates(r *RunReport) {
	add := func(name string) {
		for _, x := range r.Failures {
			if x == name {
				return
			}
		}
		r.Failures = append(r.Failures, name)
		r.Pass = false
	}
	num := func(name, key string) (float64, bool) {
		scenario, ok := r.Scenarios[name]
		if !ok {
			return 0, false
		}
		v, ok := scenario[key]
		if median, yes := scenario["median"].(map[string]any); yes {
			if mv, exists := median[key]; exists {
				v = mv
				ok = true
			}
		}
		if !ok {
			return 0, false
		}
		x, ok := v.(float64)
		if ok {
			return x, true
		}
		switch n := v.(type) {
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		case int32:
			return float64(n), true
		case uint64:
			return float64(n), true
		}
		return 0, false
	}
	if v, ok := num("discover", "discover_ms"); ok {
		if x, yes := num("discover", "spec_ms"); yes && r.Checks >= 1000 && v+x > 100 {
			add("discover")
		}
	}
	if scenario, exists := r.Scenarios["overhead"]; exists && scenario["skipped"] == true {
		// The overhead gate is only meaningful for the dedicated zero-duration
		// binary workload; the default shell workload deliberately skips it.
	} else if v, ok := num("overhead", "overhead_us"); ok && v > 2000 {
		add("overhead")
	}
	if v, ok := num("run-cold", "parallel_efficiency"); ok && r.Checks >= 100 && v < .7 {
		add("run-cold")
	}
	if scenario, exists := r.Scenarios["run-cached"]; exists {
		allCached, _ := scenario["all_cached"].(bool)
		if allCached {
			if v, ok := num("run-cached", "wall_ms"); ok && float64(v) > 50+float64(r.Checks)*.05 {
				add("run-cached")
			}
		}
	}
	if v, ok := num("join-storm", "executions"); ok && v != 1 {
		add("join-storm")
	}
	if v, ok := num("rpc-latency", "hello_p99_us"); ok && v > 5000 {
		add("rpc-latency")
	}
	if v, ok := num("rpc-latency", "run_wait_false_p99_us"); ok && v > 5000 {
		add("rpc-latency")
	}
	if v, ok := num("persist-burst", "dropped"); ok && v != 0 {
		add("persist-burst")
	}
	if v, ok := num("persist-burst", "commit_ms_p99"); ok && v > 50 {
		add("persist-burst")
	}
	if v, ok := num("persist-slowdisk", "sched_lag_ms_p99"); ok && v > 50 {
		add("persist-slowdisk")
	}
	if v, ok := num("watch-steady", "rss_mb"); ok && v > 100 {
		add("watch-steady")
	}
	if v, ok := num("watch-steady", "goroutines"); ok && math.Abs(v) > 5 {
		add("watch-steady")
	}
	if v, ok := num("restore", "restore_ms"); ok && v > 200 {
		add("restore")
	}
	if v, ok := num("prune", "prune_ms"); ok && v > 100 {
		add("prune")
	}
	if v, ok := num("tui-render", "render_ms_p99"); ok && v > 10 {
		add("tui-render")
	}
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sumOverruns(values map[string]int) int {
	total := 0
	for _, n := range values {
		total += n
	}
	return total
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	default:
		return 0, false
	}
}

func processRSSMB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			kb, err := strconv.ParseFloat(fields[1], 64)
			if err == nil {
				return kb / 1024
			}
		}
	}
	return 0
}

func metricNumber(name, key string) float64 {
	return metricNumberFrom(metrics.SnapshotAll(), name, key)
}

func metricNumberFrom(all map[string]any, name, key string) float64 {
	v, ok := all[name].(map[string]any)
	if !ok {
		return 0
	}
	n, _ := numericValue(v[key])
	return n
}

func percentile(v []time.Duration, p float64) int64 {
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	if len(v) == 0 {
		return 0
	}
	return v[int(float64(len(v)-1)*p)].Microseconds()
}
func benchCheckDuration(path string) time.Duration {
	parts := strings.Split(filepath.Base(path), "-")
	if len(parts) != 5 || parts[0] != "benchcheck" {
		return 0
	}
	n, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n)
}
func execute(ss []spec.Spec, n int) []runner.Result {
	sem := make(chan struct{}, n)
	out := make(chan runner.Result, len(ss))
	var wg sync.WaitGroup
	for _, s := range ss {
		wg.Add(1)
		go func(s spec.Spec) {
			defer wg.Done()
			sem <- struct{}{}
			out <- runner.Run(context.Background(), s, runner.Meta{Trigger: "manual", Mode: "bench", Iter: 1, Version: version.Current})
			<-sem
		}(s)
	}
	wg.Wait()
	close(out)
	var rs []runner.Result
	for x := range out {
		rs = append(rs, x)
	}
	return rs
}
func CompareFiles(base, current string, tolerance float64) ([]string, error) {
	if tolerance < 0 {
		tolerance = 10
	}
	var a, b map[string]any
	for p, v := range map[string]*map[string]any{base: &a, current: &b} {
		x, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(x, v); e != nil {
			return nil, e
		}
	}
	var bad []string
	if ap, bp := b["params"], a["params"]; ap != nil && bp != nil {
		// A baseline from a different workload is not a valid comparison.
		if fmt.Sprint(ap) != fmt.Sprint(bp) {
			return nil, fmt.Errorf("benchmark parameters differ")
		}
	}
	as, _ := a["scenarios"].(map[string]any)
	bs, _ := b["scenarios"].(map[string]any)
	if len(as) == 0 || len(bs) == 0 {
		// Accept the legacy flat discover/spec report format.
		as = map[string]any{"report": a}
		bs = map[string]any{"report": b}
	}
	for name, av := range as {
		bm, ok := bs[name].(map[string]any)
		am, ok2 := av.(map[string]any)
		if !ok || !ok2 {
			continue
		}
		if med, ok := am["median"].(map[string]any); ok {
			am = med
		}
		if med, ok := bm["median"].(map[string]any); ok {
			bm = med
		}
		for k, x := range am {
			aNum, aOK := x.(float64)
			bNum, bOK := bm[k].(float64)
			if aOK && bOK && aNum > 0 && bNum > aNum*(1+tolerance/100) {
				bad = append(bad, name+"."+k)
			}
		}
	}
	sort.Strings(bad)
	return bad, nil
}
