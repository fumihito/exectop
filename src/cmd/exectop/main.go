package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"exectop/src/aggregate"
	archiveutil "exectop/src/archive"
	"exectop/src/bench"
	"exectop/src/coordinator"
	"exectop/src/daemon"
	"exectop/src/discover"
	"exectop/src/internal/examples"
	"exectop/src/persist"
	"exectop/src/rpc"
	"exectop/src/runner"
	"exectop/src/sanitize"
	"exectop/src/spec"
	"exectop/src/status"
	"exectop/src/tui"
	"exectop/src/version"
	"flag"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

type options struct {
	root, output                string
	failOn                      string
	force, verbose, noDaemon    bool
	tree                        bool
	dbPath                      string
	timeout, interval           time.Duration
	maxAge                      time.Duration
	since                       time.Duration
	rescan                      time.Duration
	refresh                     time.Duration
	splay                       time.Duration
	tag, group, match           string
	followSymlinks, allowUnsafe bool
	limit                       int
	concurrency                 int
	inheritEnv, noState         bool
	config, stateDir            string
	archiveMode, archiveTarget  string
}

type validationReport struct {
	ID       string   `json:"id"`
	Valid    bool     `json:"valid"`
	Issues   []string `json:"issues,omitempty"`
	Requires []string `json:"requires,omitempty"`
	OS       []string `json:"requires_os,omitempty"`
	Paths    []string `json:"requires_path,omitempty"`
	Network  string   `json:"network,omitempty"`
}

var commandContext = context.Background()

func main() {
	if len(os.Args) == 1 && strings.HasPrefix(filepath.Base(os.Args[0]), "benchcheck-") {
		benchCheckFile(filepath.Base(os.Args[0]))
	}
	os.Args = defaultCommandArgs(os.Args)
	cmd := normalizeCommand(os.Args[1])
	os.Args[1] = cmd
	if cmd == "help" {
		commandHelp(os.Args[2:])
		return
	}
	if cmd == "bench-check" {
		benchCheck(os.Args[2:])
		return
	}
	if cmd == "bench" {
		benchMain(os.Args[2:])
		return
	}
	o := options{root: defaultRoot(), output: ""}
	if cmd == "examples" && o.root == "./checks" {
		o.root = "./exectop.d"
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.root, "root", o.root, "root directory")
	fs.StringVar(&o.output, "output", "", "table, json, or jsonl")
	fs.StringVar(&o.output, "o", "", "table, json, or jsonl")
	fs.BoolVar(&o.force, "force", false, "ignore minimum interval")
	fs.BoolVar(&o.verbose, "verbose", false, "show details")
	fs.BoolVar(&o.verbose, "v", false, "show details")
	fs.BoolVar(&o.noDaemon, "no-daemon", false, "run in this process")
	fs.BoolVar(&o.tree, "tree", false, "show directory rows and enable hierarchical navigation")
	fs.StringVar(&o.dbPath, "db", "", "SQLite database path")
	fs.StringVar(&o.failOn, "fail-on", "fail", "ok, warn, or fail")
	fs.DurationVar(&o.timeout, "timeout", 0, "override timeout")
	fs.DurationVar(&o.interval, "interval", 0, "override watch interval")
	fs.DurationVar(&o.maxAge, "max-age", 0, "maximum age for cached results")
	fs.DurationVar(&o.rescan, "rescan", 30*time.Second, "rescan interval (0 disables automatic rescanning)")
	fs.DurationVar(&o.refresh, "refresh", 2*time.Second, "watch table refresh interval")
	fs.DurationVar(&o.splay, "splay", 0, "initial schedule spread")
	fs.StringVar(&o.tag, "tag", "", "filter tag")
	fs.StringVar(&o.group, "group", "", "filter group")
	fs.StringVar(&o.match, "match", "", "filter ID/name regex")
	fs.BoolVar(&o.followSymlinks, "follow-symlinks", false, "follow external symlinks")
	fs.BoolVar(&o.allowUnsafe, "allow-unsafe", false, "warn instead of rejecting unsafe checks")
	fs.IntVar(&o.limit, "limit", 60, "history result limit")
	fs.IntVar(&o.limit, "history", 60, "in-memory/history result limit")
	fs.DurationVar(&o.since, "since", 0, "history lookback duration")
	defaultConcurrency := 4 * runtime.NumCPU()
	if defaultConcurrency > 32 {
		defaultConcurrency = 32
	}
	fs.IntVar(&o.concurrency, "concurrency", defaultConcurrency, "maximum concurrent checks")
	fs.IntVar(&o.concurrency, "j", defaultConcurrency, "maximum concurrent checks")
	fs.BoolVar(&o.inheritEnv, "inherit-env", false, "inherit the process environment")
	fs.BoolVar(&o.noState, "no-state", false, "disable check state directories")
	fs.StringVar(&o.config, "config", "", "global config file")
	fs.StringVar(&o.stateDir, "state-dir", "", "state directory root")
	fs.StringVar(&o.archiveMode, "mode", string(archiveutil.Refuse), "archive update mode: merge, replace, or refuse")
	fs.StringVar(&o.archiveMode, "archive-mode", string(archiveutil.Refuse), "archive update mode: merge, replace, or refuse")
	fs.StringVar(&o.archiveTarget, "target", "", "archive extraction directory")
	var commandStop context.CancelFunc
	if cmd == "run" || cmd == "oneshot" || cmd == "check" {
		commandContext, commandStop = signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer commandStop()
	}
	if cmd == "check" {
		checkArgs := normalizeCheckArgs(os.Args[2:])
		if hasHelpArg(checkArgs) {
			commandHelp([]string{cmd})
			return
		}
		if fs.Parse(checkArgs) != nil {
			os.Exit(64)
		}
		if e := validateOptions(o); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(64)
		}
		ids := fs.Args()
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "check requires an ID")
			os.Exit(64)
		}
		o.force = true
		o.verbose = true
		run(o, ids)
		return
	}
	parseArgs := os.Args[2:]
	if cmd == "archive" {
		parseArgs = normalizeArchiveArgs(parseArgs, "")
	} else if cmd == "extract" {
		parseArgs = normalizeArchiveArgs(parseArgs, "extract")
	}
	if hasHelpArg(parseArgs) {
		commandHelp([]string{cmd})
		return
	}
	var subcommand string
	if (cmd == "db" || cmd == "daemon" || cmd == "history") && len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		subcommand = parseArgs[0]
		parseArgs = parseArgs[1:]
	}
	if fs.Parse(parseArgs) != nil {
		os.Exit(64)
	}
	if e := validateOptions(o); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(64)
	}
	if o.dbPath != "" {
		o.noDaemon = true
	}
	if o.failOn != "ok" && o.failOn != "warn" && o.failOn != "fail" {
		fmt.Fprintln(os.Stderr, "invalid --fail-on:", o.failOn)
		os.Exit(64)
	}
	switch cmd {
	case "run", "oneshot":
		run(o, nil)
	case "list":
		list(o)
	case "runbook":
		runbook(o, fs.Args())
	case "watch":
		watch(o)
	case "history":
		args := fs.Args()
		if subcommand != "" {
			args = append([]string{subcommand}, args...)
		}
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "history requires check ID")
			os.Exit(64)
		}
		dbp := o.dbPath
		if dbp == "" {
			_, dbp = daemon.Paths(o.root)
		}
		db, e := persist.Open(dbp)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		defer db.Close()
		var since time.Time
		if o.since > 0 {
			since = time.Now().Add(-o.since)
		}
		rows, e := persist.HistorySince(context.Background(), db, args[0], o.limit, since)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		for _, r := range rows {
			fmt.Printf("%s\t%s\t%dms\t%s\n", r.Started.Format(time.RFC3339), r.Status, r.Duration.Milliseconds(), r.Summary)
		}
		transitions, e := persist.Transitions(context.Background(), db, args[0], since, time.Time{})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		if len(transitions) > 0 {
			fmt.Println("transitions:")
			for _, tr := range transitions {
				from := tr.FromStatus
				if from == "" {
					from = "-"
				}
				fmt.Printf("%s\t%s -> %s\n", tr.At.Format(time.RFC3339), from, tr.ToStatus)
			}
		}
	case "db":
		args := fs.Args()
		if subcommand != "" {
			args = append([]string{subcommand}, args...)
		}
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "db supports: path, vacuum, prune")
			os.Exit(64)
		}
		dbp := o.dbPath
		if dbp == "" {
			_, dbp = daemon.Paths(o.root)
		}
		if args[0] == "path" {
			fmt.Println(dbp)
			return
		}
		db, e := persist.Open(dbp)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		defer db.Close()
		if args[0] == "vacuum" {
			e = persist.Vacuum(context.Background(), db)
		} else if args[0] == "prune" {
			retention := persist.Retention{RunsMax: 1000, RunsAge: 168 * time.Hour, OutputAge: 24 * time.Hour, OutputMin: 20, TransitionsAge: 2160 * time.Hour, EventsAge: 720 * time.Hour}
			configPath := o.config
			if configPath == "" {
				configPath = filepath.Join(o.root, "exectop.toml")
			}
			if cfg, ce := spec.LoadDirectoryConfig(configPath); ce == nil {
				if rc := cfg.Sections["persist.retention"]; rc != nil {
					if v := rc["runs_max"]; v != "" {
						retention.RunsMax, _ = strconv.Atoi(v)
					}
					if v := rc["output_min"]; v != "" {
						retention.OutputMin, _ = strconv.Atoi(v)
					}
					if v := rc["runs"]; v != "" {
						retention.RunsAge, _ = time.ParseDuration(v)
					}
					if v := rc["output"]; v != "" {
						retention.OutputAge, _ = time.ParseDuration(v)
					}
					if v := rc["transitions"]; v != "" {
						retention.TransitionsAge, _ = time.ParseDuration(v)
					}
					if v := rc["events"]; v != "" {
						retention.EventsAge, _ = time.ParseDuration(v)
					}
				}
			}
			e = persist.PruneWithOptions(context.Background(), db, retention)
		} else {
			fmt.Fprintln(os.Stderr, "unknown db operation")
			os.Exit(64)
		}
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
	case "daemon":
		args := fs.Args()
		if subcommand != "" {
			args = append([]string{subcommand}, args...)
		}
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "daemon supports: run, status, stop, restart")
			os.Exit(64)
		}
		switch args[0] {
		case "run":
			d, e := daemon.NewWithOptions(o.root, daemon.Options{FollowSymlinks: o.followSymlinks, AllowUnsafe: o.allowUnsafe, ConfigPath: o.config, Rescan: o.rescan, StateDir: o.stateDir, NoState: o.noState, Concurrency: o.concurrency, History: o.limit})
			if e != nil {
				printRootError(o.root, e)
				os.Exit(70)
			}
			if e = d.Start(); e != nil {
				fmt.Fprintln(os.Stderr, e)
				os.Exit(70)
			}
			serveDaemon(d)
		case "status":
			daemonRequest(o.root, "status")
		case "stop":
			daemonRequest(o.root, "shutdown")
		case "restart":
			daemonRequest(o.root, "shutdown")
			d, e := daemon.NewWithOptions(o.root, daemon.Options{FollowSymlinks: o.followSymlinks, AllowUnsafe: o.allowUnsafe, ConfigPath: o.config, Rescan: o.rescan, StateDir: o.stateDir, NoState: o.noState, Concurrency: o.concurrency, History: o.limit})
			if e != nil {
				printRootError(o.root, e)
				os.Exit(70)
			}
			for attempt := 0; attempt < 100; attempt++ {
				e = d.Start()
				if e == nil || !strings.Contains(e.Error(), "daemon already running") {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				os.Exit(70)
			}
			serveDaemon(d)
		default:
			fmt.Fprintln(os.Stderr, "unknown daemon operation")
			os.Exit(64)
		}
	case "archive":
		if e := archiveCommand(o, fs.Args()); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(64)
		}
	case "extract":
		if e := archiveCommand(o, fs.Args()); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(64)
		}
	case "examples":
		if e := examples.Install(o.root); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(64)
		}
		fmt.Fprintf(os.Stdout, "installed examples in %s\n", filepath.Join(o.root, "examples"))
	case "check":
		run(o, fs.Args())
	case "validate":
		validateChecks(o, fs.Args())
	case "generate":
		generateCheck(o, fs.Args())
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		os.Exit(64)
	}
}

// defaultCommandArgs makes watch the default interactive behavior while
// keeping all explicit subcommands unchanged.
func defaultCommandArgs(args []string) []string {
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return append([]string{args[0], "watch"}, args[1:]...)
	}
	return args
}

func normalizeCommand(command string) string {
	if command == "top" {
		return "watch"
	}
	return command
}

func defaultRoot() string {
	if info, err := os.Stat("./exectop.d"); err == nil && info.IsDir() {
		return "./exectop.d"
	}
	return "./checks"
}

func serveDaemon(d *daemon.Daemon) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	for {
		s := <-sig
		if s == syscall.SIGHUP {
			if e := d.Reload(); e != nil {
				fmt.Fprintln(os.Stderr, e)
			}
			continue
		}
		d.Stop()
		return
	}
}
func benchMain(args []string) {
	op := "gen"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		op = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	out := fs.String("out", "./bench-root", "output directory")
	root := fs.String("root", "./bench-root", "benchmark root")
	baseline := fs.String("baseline", "", "baseline JSON")
	current := fs.String("current", "", "current JSON")
	tolerance := fs.Float64("tolerance", 10, "regression tolerance percent")
	scenario := fs.String("scenario", "", "comma-separated scenarios")
	conc := fs.Int("concurrency", 1, "benchmark concurrency")
	fs.IntVar(conc, "j", 1, "benchmark concurrency")
	jsonOut := fs.String("json", "", "write JSON report")
	n := fs.Int("checks", 500, "number of checks")
	mean := fs.Duration("mean", 200*time.Millisecond, "mean duration")
	fail := fs.Float64("fail-ratio", .1, "failure ratio")
	ob := fs.String("output-bytes", "256", "stdout bytes")
	dist := fs.String("dist", "lognormal", "fixed, uniform, lognormal, or bimodal")
	kind := fs.String("kind", "sh", "sh or bin")
	minRatio := fs.Float64("min-interval-ratio", .2, "ratio of checks with min_interval")
	intervalMin := fs.Duration("interval-min", 0, "minimum generated watch interval")
	intervalMax := fs.Duration("interval-max", 0, "maximum generated watch interval")
	seed := fs.Int64("seed", 1, "generator seed")
	repeat := fs.Int("repeat", 5, "benchmark repetitions")
	warmup := fs.Int("warmup", 1, "warmup repetitions")
	watchDuration := fs.Duration("watch-duration", time.Minute, "watch-steady measurement duration")
	noDaemon := fs.Bool("no-daemon", false, "run benchmark without daemon")
	failRegression := fs.Bool("fail-on-regression", false, "exit 1 on regression")
	if e := fs.Parse(args); e != nil {
		os.Exit(64)
	}
	rawBytes, e := bench.ParseBytes(*ob)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(64)
	}
	if op == "gen" {
		helper, _ := os.Executable()
		if e = bench.Generate(bench.GenOptions{Out: *out, Checks: *n, Mean: *mean, FailRatio: *fail, OutputBytes: rawBytes, Seed: *seed, Dist: *dist, Kind: *kind, Helper: helper, MinIntervalRatio: *minRatio, IntervalMin: *intervalMin, IntervalMax: *intervalMax}); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		return
	}
	if op == "run" || op == "all" {
		var sc []string
		if *scenario != "" {
			sc = strings.Split(*scenario, ",")
		}
		if op == "all" {
			helper, _ := os.Executable()
			if e = bench.Generate(bench.GenOptions{Out: *out, Checks: *n, Mean: *mean, FailRatio: *fail, OutputBytes: rawBytes, Seed: *seed, Dist: *dist, Kind: *kind, Helper: helper, MinIntervalRatio: *minRatio, IntervalMin: *intervalMin, IntervalMax: *intervalMax}); e != nil {
				fmt.Fprintln(os.Stderr, e)
				os.Exit(70)
			}
			*root = *out
			if len(sc) == 0 {
				sc = []string{"all"}
			}
		}
		r, e := bench.Run(bench.RunOptions{Root: *root, Concurrency: *conc, Scenarios: sc, NoDaemon: *noDaemon, Repeat: *repeat, Warmup: *warmup, WatchDuration: *watchDuration, Kind: *kind, Mean: *mean, Dist: *dist, FailRatio: *fail})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		b, e := json.Marshal(r)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		if *jsonOut != "" {
			if e := os.WriteFile(*jsonOut, b, 0600); e != nil {
				fmt.Fprintln(os.Stderr, e)
				os.Exit(70)
			}
		}
		if op == "all" && *baseline != "" {
			currentPath := *jsonOut
			removeCurrent := false
			if currentPath == "" {
				f, ce := os.CreateTemp("", "exectop-bench-current-*.json")
				if ce != nil {
					fmt.Fprintln(os.Stderr, ce)
					os.Exit(70)
				}
				currentPath = f.Name()
				_, _ = f.Write(b)
				_ = f.Close()
				removeCurrent = true
			}
			bad, ce := bench.CompareFiles(*baseline, currentPath, *tolerance)
			if removeCurrent {
				_ = os.Remove(currentPath)
			}
			if ce != nil {
				fmt.Fprintln(os.Stderr, ce)
				os.Exit(70)
			}
			for _, k := range bad {
				fmt.Println("regression:", k)
			}
			if len(bad) > 0 && *failRegression {
				os.Exit(1)
			}
		}
		fmt.Println(string(b))
		return
	}
	if op == "compare" {
		if *baseline == "" || *current == "" {
			fmt.Fprintln(os.Stderr, "compare requires --baseline and --current")
			os.Exit(64)
		}
		bad, e := bench.CompareFiles(*baseline, *current, *tolerance)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(70)
		}
		for _, k := range bad {
			fmt.Println("regression:", k)
		}
		if len(bad) > 0 && *failRegression {
			os.Exit(1)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "bench supports: gen, run, all")
	os.Exit(64)
}
func benchCheck(args []string) {
	code := 0
	if len(args) > 0 {
		code, _ = strconv.Atoi(args[0])
	}
	os.Exit(code)
}
func benchCheckFile(name string) {
	parts := strings.Split(name, "-")
	if len(parts) != 5 {
		os.Exit(1)
	}
	durationNS, e1 := strconv.ParseInt(parts[2], 10, 64)
	outputBytes, e2 := strconv.Atoi(parts[3])
	code, e3 := strconv.Atoi(parts[4])
	if e1 != nil || e2 != nil || e3 != nil || outputBytes < 0 {
		os.Exit(1)
	}
	chunk := strings.Repeat("x", 32*1024)
	for left := outputBytes; left > 0; {
		n := left
		if n > len(chunk) {
			n = len(chunk)
		}
		_, _ = os.Stdout.WriteString(chunk[:n])
		left -= n
	}
	if outputBytes > 0 {
		_, _ = os.Stdout.WriteString("\n")
	}
	if durationNS > 0 {
		time.Sleep(time.Duration(durationNS))
	}
	os.Exit(code)
}
func usage() {
	fmt.Fprintln(os.Stderr, "usage: exectop [watch] [flags]")
	fmt.Fprintln(os.Stderr, "       exectop help [COMMAND]")
	fmt.Fprintln(os.Stderr, "       exectop {watch|top|oneshot|run|list|runbook|check|validate|generate|history|daemon|db|archive|extract|examples|bench} ...")
}

func hasHelpArg(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func commandHelp(args []string) {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "help accepts at most one command")
		os.Exit(64)
	}
	if len(args) == 0 {
		usage()
		return
	}
	switch args[0] {
	case "list":
		fmt.Fprintln(os.Stderr, "exectop list [--root DIR] [--output table|json|jsonl]")
		fmt.Fprintln(os.Stderr, "  発見されたチェックと解決済みメタデータを表示します。チェックは実行しません。")
		fmt.Fprintln(os.Stderr, "  設定不備や除外理由を確認する用途です。依存性の利用可否を検査する場合は validate を使います。")
	case "validate":
		fmt.Fprintln(os.Stderr, "exectop validate [--root DIR] [--output table|json|jsonl] [CHECK_ID ...]")
		fmt.Fprintln(os.Stderr, "  チェックを実行せず、設定と宣言依存性を検証します。依存性一覧も同時に表示します。")
		fmt.Fprintln(os.Stderr, "  成功時は終了コード0、問題がある場合は終了コード1です。実測値の確認には run --no-daemon を使います。")
	case "run":
		fmt.Fprintln(os.Stderr, "exectop run [flags]")
		fmt.Fprintln(os.Stderr, "  全チェックを通常の鮮度・キャッシュポリシーで一度実行し、結果を表示します。")
		fmt.Fprintln(os.Stderr, "  指定チェックを強制実行する場合は check CHECK_ID を使います。")
	case "check":
		fmt.Fprintln(os.Stderr, "exectop check CHECK_ID ... [flags]")
		fmt.Fprintln(os.Stderr, "  指定したチェックだけを強制実行します。デバッグや単一チェックの確認用です。")
		fmt.Fprintln(os.Stderr, "  全チェックを通常ポリシーで実行する場合は run を使います。")
	case "examples":
		fmt.Fprintln(os.Stderr, "exectop examples [--root DIR]")
		fmt.Fprintln(os.Stderr, "  配布済みのサンプルチェック一式を ROOT/examples にインストールします。")
		fmt.Fprintln(os.Stderr, "  新しいチェックを作る雛形が必要な場合は generate PATH を使います。")
	case "generate":
		fmt.Fprintln(os.Stderr, "exectop generate [--root DIR] RELATIVE_PATH")
		fmt.Fprintln(os.Stderr, "  指定した相対パスに、実行可能な UNKNOWN 状態の新規チェック雛形を作成します。")
		fmt.Fprintln(os.Stderr, "  既存ファイルは上書きしません。サンプル一式を導入する場合は examples を使います。")
	default:
		fmt.Fprintln(os.Stderr, "no command help for:", args[0])
		os.Exit(64)
	}
}

func archiveCommand(o options, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("archive supports: create FILE, extract SOURCE")
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return fmt.Errorf("archive create requires output FILE")
		}
		return archiveutil.Create(o.root, args[1])
	case "extract":
		if len(args) != 2 {
			return fmt.Errorf("archive extract requires SOURCE")
		}
		target := o.archiveTarget
		if target == "" {
			target = "./exectop.d"
		}
		return archiveutil.Extract(args[1], target, archiveutil.Mode(o.archiveMode))
	default:
		return fmt.Errorf("unknown archive operation: %s", args[0])
	}
}

func normalizeArchiveArgs(args []string, implicitOperation string) []string {
	var flags, positional []string
	if implicitOperation != "" {
		positional = append(positional, implicitOperation)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		known := a == "--root" || a == "--mode" || a == "--archive-mode" || a == "--target"
		if known && i+1 < len(args) {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(a, "--root=") || strings.HasPrefix(a, "--mode=") || strings.HasPrefix(a, "--archive-mode=") || strings.HasPrefix(a, "--target=") {
			flags = append(flags, a)
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}
func found(o options) ([]discover.Found, []spec.Spec) {
	f, w, e := discover.Discover(o.root, discover.Options{FollowSymlinks: o.followSymlinks, AllowUnsafe: o.allowUnsafe})
	if e != nil {
		printRootError(o.root, e)
		os.Exit(66)
	}
	for _, x := range w {
		fmt.Fprintf(os.Stderr, "warning: %s: %s\n", x.Path, x.Reason)
	}
	sort.Slice(f, func(i, j int) bool { return f[i].ID < f[j].ID })
	ss := make([]spec.Spec, 0, len(f))
	configPath := o.config
	if configPath == "" {
		configPath = filepath.Join(o.root, "exectop.toml")
	}
	globalConfig, globalConfigErr := spec.LoadDirectoryConfig(configPath)
	for _, x := range f {
		m, he := spec.ParseHeader(x.Path)
		if he != nil {
			s := spec.Default(x)
			s.InvalidReason = he.Error()
			ss = append(ss, s)
			continue
		}
		layers, ce := spec.ChainWithHeader(o.root, x.Path, x.ID, m)
		if ce != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", x.ID, ce)
			s := spec.Default(x)
			s.InvalidReason = ce.Error()
			ss = append(ss, s)
			continue
		}
		if globalConfigErr != nil {
			s := spec.Default(x)
			s.InvalidReason = globalConfigErr.Error()
			ss = append(ss, s)
			continue
		}
		global := map[string]string{}
		if globalConfigErr == nil {
			for k, v := range globalConfig.Defaults {
				global[k] = v
			}
			for k, v := range globalConfig.Sections["global"] {
				global[k] = v
			}
		}
		s, e := spec.Resolve(x, layers, global, spec.Overrides{Timeout: o.timeout, Interval: o.interval, HasTimeout: o.timeout > 0, HasInterval: o.interval > 0, AllowUnsafe: o.allowUnsafe})
		if e != nil {
			s = spec.Default(x)
			s.InvalidReason = e.Error()
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", x.ID, e)
		}
		for _, warning := range s.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s: %s\n", x.ID, warning)
		}
		ss = append(ss, s)
	}
	if o.tag != "" || o.group != "" || o.match != "" {
		var filtered []spec.Spec
		var re *regexp.Regexp
		if o.match != "" {
			var e error
			re, e = regexp.Compile(o.match)
			if e != nil {
				fmt.Fprintln(os.Stderr, "invalid --match:", e)
				os.Exit(64)
			}
		}
		for _, s := range ss {
			if o.group != "" {
				foundGroup := s.ID
				if i := strings.IndexByte(foundGroup, '/'); i >= 0 {
					foundGroup = foundGroup[:i]
				} else {
					foundGroup = ""
				}
				if foundGroup != o.group {
					continue
				}
			}
			if o.tag != "" && !containsString(s.Tags, o.tag) {
				continue
			}
			if re != nil && !re.MatchString(s.ID) && !re.MatchString(s.Name) {
				continue
			}
			filtered = append(filtered, s)
		}
		ss = filtered
	}
	return f, ss
}

func printRootError(root string, err error) {
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "root directory does not exist: %s\n", root)
		fmt.Fprintln(os.Stderr, "run `exectop examples` to create the bundled example checks")
		return
	}
	fmt.Fprintln(os.Stderr, err)
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func list(o options) {
	_, ss := found(o)
	if o.output == "json" || o.output == "jsonl" {
		if o.output == "json" {
			b, _ := json.Marshal(map[string]any{"checks": ss})
			fmt.Println(string(b))
			return
		}
		for _, s := range ss {
			b, _ := json.Marshal(s)
			fmt.Println(string(b))
		}
		return
	}
	for _, s := range ss {
		fmt.Printf("%s\t%s\t%s\t%s\n", s.ID, s.Name, s.Interval, s.Hash)
	}
}

func validateChecks(o options, ids []string) {
	_, specs := found(o)
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	reports := make([]validationReport, 0, len(specs))
	checked := 0
	for _, s := range specs {
		if len(wanted) > 0 && !wanted[s.ID] {
			continue
		}
		checked++
		report := validationReport{
			ID: s.ID, Valid: true, Requires: s.Requires, OS: s.RequiresOS,
			Paths: s.RequiresPath, Network: s.Network,
		}
		if s.InvalidReason != "" {
			report.Valid = false
			report.Issues = append(report.Issues, s.InvalidReason)
		} else if reason := runner.CheckDependencies(s); reason != "" {
			report.Valid = false
			report.Issues = append(report.Issues, reason)
		}
		reports = append(reports, report)
	}
	if o.output == "json" || o.output == "jsonl" {
		if o.output == "jsonl" {
			for _, report := range reports {
				b, _ := json.Marshal(report)
				fmt.Println(string(b))
			}
		} else {
			b, _ := json.Marshal(map[string]any{"checked": checked, "valid": reportsValid(reports), "checks": reports})
			fmt.Println(string(b))
		}
	} else {
		for _, report := range reports {
			state := "valid"
			if !report.Valid {
				state = "INVALID"
			}
			fmt.Printf("%s\t%s\tcommands=%s\tos=%s\tpaths=%s\tnetwork=%s\n", report.ID, state,
				strings.Join(report.Requires, ","), strings.Join(report.OS, ","),
				strings.Join(report.Paths, ","), report.Network)
			for _, issue := range report.Issues {
				fmt.Printf("%s: %s\n", report.ID, issue)
			}
		}
		fmt.Printf("valid: %d/%d checks\n", countValidReports(reports), checked)
	}
	if !reportsValid(reports) {
		os.Exit(1)
	}
}

func reportsValid(reports []validationReport) bool {
	for _, report := range reports {
		if !report.Valid {
			return false
		}
	}
	return true
}

func countValidReports(reports []validationReport) int {
	count := 0
	for _, report := range reports {
		if report.Valid {
			count++
		}
	}
	return count
}

func generateCheck(o options, args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "generate requires one relative check path")
		os.Exit(64)
	}
	id := filepath.ToSlash(args[0])
	cleanID := filepath.ToSlash(filepath.Clean(filepath.FromSlash(id)))
	if id == "" || filepath.IsAbs(id) || id == "." || cleanID != id || strings.HasPrefix(id, "../") || strings.Contains(id, "/../") {
		fmt.Fprintln(os.Stderr, "generate path must stay below root")
		os.Exit(64)
	}
	root, err := filepath.Abs(o.root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(64)
	}
	path := filepath.Join(root, filepath.FromSlash(id))
	if _, err = os.Stat(path); err == nil {
		fmt.Fprintln(os.Stderr, "generate target already exists:", path)
		os.Exit(73)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(73)
	}
	body := "#!/bin/sh\nset -eu\n# exectop: short_name=\"" + filepath.Base(path) + "\" description=\"TODO: describe this check\" interval=1m timeout=5s\n\n# Replace this UNKNOWN result with a real measurement and threshold.\nprintf '%s\\n' '{\"summary\":\"not implemented\",\"value\":0,\"unit\":\"unknown\"}'\nexit 3\n"
	if err = os.WriteFile(path, []byte(body), 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(73)
	}
	fmt.Println(path)
}

func runbook(o options, args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "runbook requires check ID")
		os.Exit(64)
	}
	_, ss := found(o)
	for _, s := range ss {
		if s.ID != args[0] {
			continue
		}
		if s.Runbook == "" {
			fmt.Fprintln(os.Stderr, "no runbook configured for:", s.ID)
			os.Exit(1)
		}
		if o.output == "json" || o.output == "jsonl" {
			b, _ := json.Marshal(map[string]string{"id": s.ID, "runbook": s.Runbook})
			fmt.Println(string(b))
		} else {
			fmt.Println(s.Runbook)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "unknown check:", args[0])
	os.Exit(64)
}

func run(o options, ids []string) {
	if !o.noDaemon {
		runDaemon(o, ids)
		return
	}
	_, ss := found(o)
	if len(ids) > 0 {
		known := map[string]bool{}
		for _, s := range ss {
			known[s.ID] = true
		}
		for _, id := range ids {
			if !known[id] {
				fmt.Fprintln(os.Stderr, "unknown check:", id)
				os.Exit(64)
			}
		}
	}
	if o.dbPath != "" {
		runStandaloneDB(o, ss, ids)
		return
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var rs []runner.Result
	for _, s := range ss {
		if len(ids) > 0 && !wanted[s.ID] {
			continue
		}
		m := runner.Meta{Trigger: "request", Mode: "run", Iter: 1, Version: version.Current, InheritEnv: o.inheritEnv || s.InheritEnv}
		if !o.noState {
			base := o.stateDir
			if base == "" {
				_, dbp := daemon.Paths(o.root)
				base = filepath.Join(filepath.Dir(dbp), "state")
			}
			m.StateDir = filepath.Join(base, strings.ReplaceAll(s.ID, "/", "%2F"))
		}
		rs = append(rs, runner.Run(commandContext, s, m))
	}
	if e := render(o, rs); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	if commandContext.Err() != nil {
		os.Exit(130)
	}
	os.Exit(aggregate.Exit(rs, o.failOn))
}

func runStandaloneDB(o options, ss []spec.Spec, ids []string) {
	if e := os.MkdirAll(filepath.Dir(o.dbPath), 0700); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	lf, e := os.OpenFile(filepath.Join(filepath.Dir(o.dbPath), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e == nil {
		e = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			defer func() {
				_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
				_ = lf.Close()
			}()
		} else {
			_ = lf.Close()
		}
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, "database is owned by a daemon:", e)
		os.Exit(70)
	}
	db, e := persist.Open(o.dbPath)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	defer db.Close()
	st := coordinator.NewStore()
	c := coordinator.New(st, func(ctx context.Context, s spec.Spec, m runner.Meta) runner.Result {
		if !o.noState {
			base := o.stateDir
			if base == "" {
				base = filepath.Join(filepath.Dir(o.dbPath), "state")
			}
			m.StateDir = filepath.Join(base, strings.ReplaceAll(s.ID, "/", "%2F"))
		}
		return runner.Run(ctx, s, m)
	})
	if o.concurrency > 0 {
		c.SetConcurrency(o.concurrency)
	}
	st.SetHistoryLimit(o.limit)
	for _, s := range ss {
		c.Add(s)
		_ = persist.UpsertCheck(context.Background(), db, s, groupOf(s.ID))
	}
	all := make([]string, 0, len(ss))
	for _, s := range ss {
		all = append(all, s.ID)
	}
	if rows, e := persist.LoadLatest(context.Background(), db, all); e == nil {
		for id, v := range rows {
			st.Restore(id, v.Result)
		}
	}
	w := persist.NewWriter(db, 4096)
	defer w.Close(context.Background())
	c.SetResultHook(func(r runner.Result) { w.Enqueue(persist.Item{Result: r}) })
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	var rs []runner.Result
	for _, s := range ss {
		if len(ids) > 0 && !wanted[s.ID] {
			continue
		}
		leased, e := persist.TryLease(context.Background(), db, s.ID, time.Now().Add(s.Timeout+s.Grace))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			continue
		}
		if !leased {
			old, ok := st.Get(s.ID)
			if !ok || old.Latest == nil {
				// Another --no-daemon process may have acquired the lease and
				// not committed its first result yet. Wait for that result so a
				// concurrent caller does not silently omit the check.
				deadline := time.Now().Add(s.Timeout + s.Grace + time.Second)
				for time.Now().Before(deadline) {
					rows, le := persist.LoadLatest(context.Background(), db, []string{s.ID})
					if le == nil {
						if row, exists := rows[s.ID]; exists {
							st.Restore(s.ID, row.Result)
							old, ok = st.Get(s.ID)
							break
						}
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if ok && old.Latest != nil {
				r := *old.Latest
				r.Cached = true
				r.Age = time.Since(r.Start)
				effective := s.MinInterval
				if status.IsFailure(r.Status) {
					effective = s.MinIntervalFail
				}
				r.NextEligible = r.Start.Add(effective)
				if o.maxAge > 0 && r.Age > o.maxAge {
					r.Status = status.Unknown
					r.Reason = "cached result exceeded max age"
				}
				rs = append(rs, r)
			}
			continue
		}
		v, e := c.Request(commandContext, s.ID, coordinator.Freshness{Force: o.force, MaxAge: o.maxAge, InheritEnv: o.inheritEnv, Timeout: o.timeout, Interval: o.interval}, "request")
		_ = persist.ReleaseLease(context.Background(), db, s.ID)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			continue
		}
		rs = append(rs, v.Result)
	}
	if e := render(o, rs); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	if commandContext.Err() != nil {
		os.Exit(130)
	}
	os.Exit(aggregate.Exit(rs, o.failOn))
}
func groupOf(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return ""
}

func runDaemon(o options, ids []string) {
	rs, e := fetchDaemon(o, ids)
	if e != nil {
		if commandContext.Err() != nil {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	if e := render(o, rs); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	if commandContext.Err() != nil {
		os.Exit(130)
	}
	os.Exit(aggregate.Exit(rs, o.failOn))
}
func fetchDaemon(o options, ids []string) ([]runner.Result, error) {
	if commandContext.Err() != nil {
		return nil, commandContext.Err()
	}
	if ids == nil && (o.tag != "" || o.group != "" || o.match != "") {
		_, ss := found(o)
		ids = make([]string, 0, len(ss))
		for _, s := range ss {
			ids = append(ids, s.ID)
		}
	}
	sock, _ := daemon.Paths(o.root)
	var c net.Conn
	var e error
	for i := 0; i < 150; i++ {
		c, e = net.Dial("unix", sock)
		if e == nil {
			break
		}
		if i == 0 {
			exe, _ := os.Executable()
			if st, e := os.Stat(exe); e != nil || st.Mode().Perm()&0022 != 0 {
				if e != nil {
					return nil, e
				}
				return nil, fmt.Errorf("refusing unsafe executable: %s", exe)
			}
			p := daemonLaunchArgs(o)
			_, _ = os.StartProcess(exe, append([]string{exe}, p...), &os.ProcAttr{Files: []*os.File{nil, nil, nil}, Env: launcherEnv(), Sys: &syscall.SysProcAttr{Setsid: true}})
		}
		select {
		case <-commandContext.Done():
			return nil, commandContext.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if e != nil {
		return nil, fmt.Errorf("daemon unavailable: %w", e)
	}
	defer c.Close()
	reader := rpc.NewReader(c)
	closed := make(chan struct{})
	go func() {
		select {
		case <-commandContext.Done():
			_ = c.Close()
		case <-closed:
		}
	}()
	defer close(closed)
	hello, _ := json.Marshal(map[string]any{"client_version": version.Current, "proto_version": 1, "exe_mtime": executableMtime()})
	if e = rpc.Write(c, rpc.Request{ID: 0, Method: "hello", Params: hello}); e != nil {
		return nil, e
	}
	if rawHello, he := reader.Read(); he != nil {
		return nil, he
	} else {
		var hr rpc.Response
		if json.Unmarshal(rawHello, &hr) == nil && hr.Error != nil {
			if hr.Error.Code == "client_newer" {
				c.Close()
				time.Sleep(100 * time.Millisecond)
				return fetchDaemon(o, ids)
			}
			return nil, fmt.Errorf("daemon hello: %s", hr.Error.Message)
		}
	}
	p := map[string]any{"ids": ids, "force": o.force, "max_age": o.maxAge, "inherit_env": o.inheritEnv, "wait": true, "timeout": o.timeout, "interval": o.interval}
	b, _ := json.Marshal(p)
	if e = rpc.Write(c, rpc.Request{ID: 1, Method: "run", Params: b}); e != nil {
		return nil, e
	}
	var rs []runner.Result
	for {
		raw, re := reader.Read()
		if re != nil {
			return nil, re
		}
		var resp rpc.Response
		if re = json.Unmarshal(raw, &resp); re != nil {
			return nil, re
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		if resp.Result != nil {
			b, _ := json.Marshal(resp.Result)
			var v struct {
				Result runner.Result `json:"result"`
			}
			if json.Unmarshal(b, &v) == nil && v.Result.ID != "" {
				rs = append(rs, v.Result)
			}
		}
		if resp.Final {
			break
		}
	}
	// The daemon may complete checks concurrently, but the public run
	// contract is discovery order (Discover and filteredEntries are sorted by
	// check ID). Keep output deterministic for table, JSON, and aggregation.
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	return rs, nil
}

func daemonRequest(root, method string) {
	daemonRequestOutput(root, method, true)
}
func daemonCall(root, method string) error {
	sock, _ := daemon.Paths(root)
	c, e := net.Dial("unix", sock)
	if e != nil {
		return e
	}
	defer c.Close()
	reader := rpc.NewReader(c)
	if e = writeDaemonHello(c); e != nil {
		return e
	}
	if e = rpc.Write(c, rpc.Request{ID: 1, Method: method}); e != nil {
		return e
	}
	b, e := reader.Read()
	if e != nil {
		return e
	}
	var r rpc.Response
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	if r.Error != nil {
		return fmt.Errorf("%s", r.Error.Message)
	}
	return nil
}
func daemonRequestOutput(root, method string, output bool) {
	sock, _ := daemon.Paths(root)
	c, e := net.Dial("unix", sock)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	defer c.Close()
	reader := rpc.NewReader(c)
	if e = writeDaemonHello(c); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	if e = rpc.Write(c, rpc.Request{ID: 1, Method: method}); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	b, e := reader.Read()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(70)
	}
	if output {
		fmt.Println(string(b))
	}
}

func writeDaemonHello(c net.Conn) error {
	b, _ := json.Marshal(map[string]any{"client_version": version.Current, "proto_version": 1, "exe_mtime": executableMtime()})
	if e := rpc.Write(c, rpc.Request{ID: 0, Method: "hello", Params: b}); e != nil {
		return e
	}
	raw, e := rpc.Read(c)
	if e != nil {
		return e
	}
	var resp rpc.Response
	if e = json.Unmarshal(raw, &resp); e != nil {
		return e
	}
	if resp.Error != nil {
		return fmt.Errorf("daemon hello: %s", resp.Error.Message)
	}
	return nil
}

func watch(o options) {
	if !o.noDaemon {
		if st, e := os.Stdout.Stat(); e == nil && st.Mode()&os.ModeCharDevice != 0 && o.output != "jsonl" {
			app := tui.NewAppWithEvents(func(ctx context.Context, ids []string, force bool) ([]runner.Result, error) {
				oo := o
				oo.force = force
				return fetchDaemon(oo, ids)
			}, func(ctx context.Context) (<-chan tui.WatchEvent, error) { return subscribeDaemon(o) })
			app.SetControl(func(ctx context.Context, method string) error { return daemonCall(o.root, method) })
			app.SetColumnResolver(func(dir string) []string {
				columns, err := spec.LoadColumns(o.root, dir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: columns: %v\n", err)
					return spec.DefaultColumns
				}
				return columns
			})
			app.SetHistory(func(ctx context.Context, id string) ([]string, error) {
				_, dbp := daemon.Paths(o.root)
				db, e := persist.Open(dbp)
				if e != nil {
					return nil, e
				}
				defer db.Close()
				rows, e := persist.History(ctx, db, id, o.limit)
				if e != nil {
					return nil, e
				}
				out := make([]string, 0, len(rows))
				for _, r := range rows {
					out = append(out, fmt.Sprintf("%s %s %dms %s", r.Started.Format(time.RFC3339), r.Status, r.Duration.Milliseconds(), sanitize.Text([]byte(r.Summary))))
				}
				transitions, e := persist.Transitions(ctx, db, id, time.Time{}, time.Time{})
				if e != nil {
					return nil, e
				}
				for _, tr := range transitions {
					from := tr.FromStatus
					if from == "" {
						from = "-"
					}
					out = append(out, fmt.Sprintf("transition %s %s -> %s", tr.At.Format(time.RFC3339), from, tr.ToStatus))
				}
				return out, nil
			})
			_, ss := found(o)
			for _, s := range ss {
				app.AddSpec(s)
			}
			app.SetFlat(!o.tree)
			clearTerminal()
			if _, e := tea.NewProgram(app).Run(); e != nil {
				fmt.Fprintln(os.Stderr, e)
			}
			return
		}
	}
	if !o.noDaemon {
	watchReconnect:
		for {
			ch, e := subscribeDaemon(o)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				time.Sleep(time.Second)
				continue
			}
			lastRescan := time.Now()
			if o.output == "table" {
				latest := map[string]runner.Result{}
				refresh := o.refresh
				if refresh <= 0 {
					refresh = 2 * time.Second
				}
				tick := time.NewTicker(refresh)
				for {
					select {
					case event, ok := <-ch:
						if !ok {
							tick.Stop()
							continue watchReconnect
						}
						if event.Result != nil {
							latest[event.Result.ID] = *event.Result
						}
					case <-tick.C:
						rs := make([]runner.Result, 0, len(latest))
						for _, r := range latest {
							rs = append(rs, r)
						}
						if renderWatch(o, rs) != nil {
							tick.Stop()
							return
						}
						if o.rescan > 0 && time.Since(lastRescan) >= o.rescan {
							daemonRequestOutput(o.root, "rescan", false)
							lastRescan = time.Now()
						}
					}
				}
			}
			for event := range ch {
				if event.Result == nil {
					continue
				}
				r := *event.Result
				if o.rescan > 0 && time.Since(lastRescan) >= o.rescan {
					daemonRequestOutput(o.root, "rescan", false)
					lastRescan = time.Now()
				}
				if renderWatch(o, []runner.Result{r}) != nil {
					return
				}
			}
			continue watchReconnect
		}
	}
	runWatchStandalone(o)
}
func subscribeDaemon(o options) (<-chan tui.WatchEvent, error) {
	sock, _ := daemon.Paths(o.root)
	var c net.Conn
	var e error
	for i := 0; i < 150; i++ {
		c, e = net.Dial("unix", sock)
		if e == nil {
			break
		}
		if i == 0 {
			exe, ee := os.Executable()
			if ee != nil {
				return nil, ee
			}
			st, ee := os.Stat(exe)
			if ee != nil {
				return nil, ee
			}
			if st.Mode().Perm()&0022 != 0 {
				return nil, fmt.Errorf("refusing unsafe executable: %s", exe)
			}
			p := daemonLaunchArgs(o)
			if _, ee = os.StartProcess(exe, append([]string{exe}, p...), &os.ProcAttr{Files: []*os.File{nil, nil, nil}, Env: launcherEnv(), Sys: &syscall.SysProcAttr{Setsid: true}}); ee != nil {
				return nil, ee
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e != nil {
		return nil, e
	}
	reader := rpc.NewReader(c)
	b, _ := json.Marshal(map[string]any{"snapshot": true, "splay": o.splay, "rescan": o.rescan, "inherit_env": o.inheritEnv, "interval": o.interval, "timeout": o.timeout, "filter": map[string]any{"tag": o.tag, "group": o.group, "match": o.match}})
	hello, _ := json.Marshal(map[string]any{"client_version": version.Current, "proto_version": 1, "exe_mtime": executableMtime()})
	if e = rpc.Write(c, rpc.Request{ID: 0, Method: "hello", Params: hello}); e != nil {
		c.Close()
		return nil, e
	}
	if rawHello, he := reader.Read(); he != nil {
		c.Close()
		return nil, he
	} else {
		var hr rpc.Response
		if json.Unmarshal(rawHello, &hr) == nil && hr.Error != nil {
			if hr.Error.Code == "client_newer" {
				c.Close()
				return nil, fmt.Errorf("daemon restarting for newer client")
			}
			c.Close()
			return nil, fmt.Errorf("daemon hello: %s", hr.Error.Message)
		}
	}
	if e = rpc.Write(c, rpc.Request{ID: 2, Method: "subscribe", Params: b}); e != nil {
		c.Close()
		return nil, e
	}
	ch := make(chan tui.WatchEvent, 64)
	go func() {
		defer close(ch)
		defer c.Close()
		for {
			raw, e := reader.Read()
			if e != nil {
				return
			}
			var resp rpc.Response
			if json.Unmarshal(raw, &resp) != nil {
				continue
			}
			x, _ := json.Marshal(resp.Result)
			var snapshot []struct {
				ID      string `json:"id"`
				Status  string `json:"status"`
				State   string `json:"state"`
				Changed bool   `json:"changed"`
			}
			if json.Unmarshal(x, &snapshot) == nil && len(snapshot) > 0 {
				for _, item := range snapshot {
					if item.ID != "" {
						r := runner.Result{ID: item.ID, Status: status.Parse(item.Status), Changed: item.Changed}
						ch <- tui.WatchEvent{Result: &r}
					}
				}
				continue
			}
			var v struct {
				State  string        `json:"state"`
				Spec   *spec.Spec    `json:"spec"`
				Result runner.Result `json:"result"`
			}
			if json.Unmarshal(x, &v) == nil {
				if v.State == "added" && v.Spec != nil {
					ch <- tui.WatchEvent{State: v.State, Spec: v.Spec}
				} else if v.Result.ID != "" {
					ch <- tui.WatchEvent{State: v.State, Result: &v.Result}
				}
			}
		}
	}()
	return ch, nil
}
func runWatchStandalone(o options) {
	_, ss := found(o)
	currentSpecs := make(map[string]string, len(ss))
	for _, s := range ss {
		currentSpecs[s.ID] = watchSpecFingerprint(s)
	}
	latest := make(map[string]runner.Result, len(ss))
	c := coordinator.New(nil, func(ctx context.Context, s spec.Spec, m runner.Meta) runner.Result {
		if !o.noState {
			base := o.stateDir
			if base == "" {
				_, dbp := daemon.Paths(o.root)
				base = filepath.Join(filepath.Dir(dbp), "state")
			}
			m.StateDir = filepath.Join(base, strings.ReplaceAll(s.ID, "/", "%2F"))
		}
		return runner.Run(ctx, s, m)
	})
	if o.concurrency > 0 {
		c.SetConcurrency(o.concurrency)
	}
	for _, s := range ss {
		c.Add(s)
	}
	interval := o.refresh
	if interval <= 0 {
		interval = time.Second
	}
	nextRun := make(map[string]time.Time, len(ss))
	onceDone := make(map[string]bool, len(ss))
	var nextMu sync.Mutex
	lastRescan := time.Now()
	for {
		if o.rescan > 0 && time.Since(lastRescan) >= o.rescan {
			_, fresh := found(o)
			previousSpecs := currentSpecs
			currentSpecs = make(map[string]string, len(fresh))
			for _, s := range fresh {
				currentSpecs[s.ID] = watchSpecFingerprint(s)
				if old, ok := previousSpecs[s.ID]; ok && old != currentSpecs[s.ID] {
					if r, ok := latest[s.ID]; ok {
						r.Changed = true
						r.ChangedAt = time.Now()
						latest[s.ID] = r
					}
				}
			}
			present := make(map[string]bool, len(fresh))
			for _, s := range fresh {
				present[s.ID] = true
				c.Add(s)
			}
			for _, old := range ss {
				if !present[old.ID] {
					c.Remove(old.ID)
				}
			}
			ss = fresh
			lastRescan = time.Now()
		}
		out := make(chan runner.Result, len(ss))
		var wg sync.WaitGroup
		now := time.Now()
		for _, s := range ss {
			s := s
			nextMu.Lock()
			next := nextRun[s.ID]
			once := onceDone[s.ID]
			nextMu.Unlock()
			if once && s.RunOnce {
				continue
			}
			if !o.force && !next.IsZero() && now.Before(next) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, e := c.Request(context.Background(), s.ID, coordinator.Freshness{Force: o.force, MaxAge: o.maxAge, InheritEnv: o.inheritEnv || s.InheritEnv, Timeout: o.timeout, Interval: o.interval}, "schedule")
				if e == nil {
					out <- v.Result
					period := s.Interval
					if o.interval > 0 {
						period = o.interval
					}
					if period <= 0 {
						period = time.Second
					}
					base := v.Result.Start
					if base.IsZero() {
						base = time.Now()
					}
					next := base.Add(period)
					if v.Cached && !v.NextEligible.IsZero() && v.NextEligible.After(next) {
						next = v.NextEligible
					}
					nextMu.Lock()
					nextRun[s.ID] = next
					if s.RunOnce {
						onceDone[s.ID] = true
					}
					nextMu.Unlock()
				}
			}()
		}
		wg.Wait()
		close(out)
		var rs []runner.Result
		for v := range out {
			rs = append(rs, v)
			latest[v.ID] = v
		}
		if o.output == "table" || (o.output == "" && stdoutTerminal()) {
			rs = rs[:0]
			for _, v := range latest {
				rs = append(rs, v)
			}
		}
		if renderWatch(o, rs) != nil {
			return
		}
		time.Sleep(interval)
	}
}

// renderWatch redraws table output in place when watch is attached to a
// terminal. Structured output and redirected table output remain append-only.
func stdoutTerminal() bool {
	st, e := os.Stdout.Stat()
	return e == nil && st.Mode()&os.ModeCharDevice != 0
}

func watchSpecFingerprint(s spec.Spec) string {
	stamp, size := int64(0), int64(0)
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

func renderWatch(o options, rs []runner.Result) error {
	format := o.output
	terminal := stdoutTerminal()
	if format == "" && terminal {
		format = "table"
	}
	if format == "table" && terminal {
		if e := clearTerminal(); e != nil {
			return e
		}
	}
	return render(o, rs)
}

// clearTerminal gives watch the same initial full-screen behavior as top.
// It is deliberately used only for interactive terminal output; redirected
// and structured output must remain suitable for logs and pipelines.
func clearTerminal() error {
	_, err := fmt.Fprint(os.Stdout, "\033[2J\033[H")
	return err
}

func render(o options, rs []runner.Result) error {
	format := o.output
	if format == "" {
		if st, e := os.Stdout.Stat(); e == nil && st.Mode()&os.ModeCharDevice != 0 {
			format = "table"
		} else {
			format = "jsonl"
		}
	}
	switch format {
	case "jsonl":
		for _, r := range rs {
			b, _ := json.Marshal(r)
			if _, e := fmt.Fprintln(os.Stdout, string(b)); e != nil {
				return e
			}
		}
	case "json":
		counts := map[string]int{"executed": 0, "cached": 0, "joined": 0}
		for _, r := range rs {
			if r.Cached {
				counts["cached"]++
			} else if r.Joined {
				counts["joined"]++
			} else {
				counts["executed"]++
			}
		}
		ended := time.Now()
		var daemonPID any
		if !o.noDaemon {
			if pid := queryDaemonPID(o.root); pid > 0 {
				daemonPID = pid
			}
		}
		b, _ := json.Marshal(map[string]any{
			"exectop_version": version.Current,
			"root":            o.root,
			"daemon_pid":      daemonPID,
			"started_at":      rsStarted(rs, ended),
			"ended_at":        ended,
			"run":             map[string]any{"count": len(rs), "counts": counts, "exit_code": aggregate.Exit(rs, o.failOn)},
			"results":         rs,
		})
		if _, e := fmt.Fprintln(os.Stdout, string(b)); e != nil {
			return e
		}
	case "table":
		var table strings.Builder
		tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
		columns, columnErr := spec.LoadColumns(o.root, "")
		if columnErr != nil {
			fmt.Fprintf(os.Stderr, "warning: columns: %v\n", columnErr)
			columns = spec.DefaultColumns
		}
		header := make([]string, len(columns))
		for i, key := range columns {
			header[i] = strings.ToUpper(key)
		}
		if _, e := fmt.Fprintln(tw, strings.Join(header, "\t")); e != nil {
			return e
		}
		counts := map[status.Status]int{}
		executed, cached, joined := 0, 0, 0
		var sum time.Duration
		var first, last time.Time
		for _, r := range rs {
			name := displayShortName(r.Name, r.ID)
			lastText := "now"
			if r.Cached {
				lastText = r.Age.Round(time.Millisecond).String() + " ago (cached)"
			} else if r.Joined {
				lastText = "now (joined)"
			}
			code := "-"
			if r.ExitCode >= 0 {
				code = strconv.Itoa(r.ExitCode)
			}
			statusText := string(r.Status)
			if r.Changed {
				statusText += "*"
			}
			cells := make([]string, len(columns))
			for i, key := range columns {
				cells[i] = watchTableCell(key, r, statusText, name, lastText, code)
			}
			if _, e := fmt.Fprintln(tw, strings.Join(cells, "\t")); e != nil {
				return e
			}
			if o.verbose {
				if e := tw.Flush(); e != nil {
					return e
				}
				if _, e := fmt.Fprintf(&table, "  stdout: %s\n  stderr: %s\n", sanitize.Text(r.Stdout), sanitize.Text(r.Stderr)); e != nil {
					return e
				}
			}
			counts[r.Status]++
			if r.Cached {
				cached++
			} else if r.Joined {
				joined++
			} else {
				executed++
			}
			sum += r.Duration
			// Cached responses carry the original execution timestamps. They
			// must not make this invocation's wall time span back to that run.
			if !r.Cached && !r.Start.IsZero() && (first.IsZero() || r.Start.Before(first)) {
				first = r.Start
			}
			if !r.Cached && r.End.After(last) {
				last = r.End
			}
		}
		if e := tw.Flush(); e != nil {
			return e
		}
		output := table.String()
		if stdoutTerminal() {
			output = colorizeTable(output)
		}
		if _, e := os.Stdout.WriteString(output); e != nil {
			return e
		}
		wall := time.Duration(0)
		if !first.IsZero() && !last.IsZero() && last.After(first) {
			wall = last.Sub(first)
		}
		if _, e := fmt.Fprintf(os.Stdout, "----\n%d checks: %d ok, %d warn, %d fail, %d timeout  (%d executed, %d cached, %d joined; wall %s, sum %s)\n", len(rs), counts[status.OK], counts[status.Warn], counts[status.Fail]+counts[status.Error], counts[status.Timeout], executed, cached, joined, formatDuration(wall), formatDuration(sum)); e != nil {
			return e
		}
	default:
		return fmt.Errorf("unsupported output format: %s", format)
	}
	return nil
}

// colorizeTable uses only ANSI 16-color sequences. The status marker is
// intentionally textual as well as colored, so the output remains
// distinguishable for users who cannot rely on hue.
func colorizeTable(table string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(table, "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if trimmed == "" {
			b.WriteString(line)
			continue
		}
		if strings.HasPrefix(trimmed, "ST ") {
			b.WriteString("\033[1m" + trimmed + "\033[0m\n")
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			b.WriteString(line)
			continue
		}
		statusText := fields[0]
		marker, color := tableStatusStyle(statusText)
		if marker == "" {
			b.WriteString(line)
			continue
		}
		// Keep the table's spacing intact and color the complete row for
		// failures, making errors visible even when status text is skipped.
		styled := marker + trimmed
		if color == "\033[31m" {
			styled = "\033[1;7;31m" + styled + "\033[0m"
		} else {
			styled = color + styled + "\033[0m"
		}
		b.WriteString(styled + "\n")
	}
	return b.String()
}

func tableStatusStyle(s string) (string, string) {
	base := strings.TrimSuffix(s, "*")
	switch base {
	case string(status.OK):
		return "+ ", "\033[32m"
	case string(status.Warn):
		return "! ", "\033[33m"
	case string(status.Fail), string(status.Timeout), string(status.Error), string(status.Vanished):
		return "X ", "\033[31m"
	case string(status.Unknown):
		return "? ", "\033[35m"
	case string(status.Running):
		return "> ", "\033[36m"
	case string(status.Pending):
		return ". ", "\033[36m"
	case string(status.Skip):
		return "- ", "\033[37m"
	default:
		return "", ""
	}
}

func displayShortName(name, path string) string {
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(path)
	}
	runes := []rune(strings.TrimSpace(name))
	if len(runes) > 20 {
		runes = runes[:20]
	}
	return string(runes)
}

func watchTableCell(key string, r runner.Result, st, name, last, code string) string {
	values := map[string]string{
		"status": st, "s": st, "name": name, "n": name,
		"summary": r.Summary, "m": r.Summary, "interval": "-", "i": "-", "code": code,
		"history": "-", "t": "-", "group": r.Group, "g": r.Group,
		"last": last, "l": last, "duration": formatDuration(r.Duration), "d": formatDuration(r.Duration),
		"path": r.ID,
	}
	v := values[strings.ToLower(key)]
	if v == "" && r.Variables != nil {
		if x, ok := r.Variables[key]; ok {
			b, _ := json.Marshal(x)
			v = string(b)
		}
	}
	if v == "" {
		v = "-"
	}
	return sanitize.Field([]byte(v))
}

func validOutput(format string) bool {
	return format == "" || format == "table" || format == "json" || format == "jsonl"
}

func validateOptions(o options) error {
	if !validOutput(o.output) {
		return fmt.Errorf("invalid --output: %s", o.output)
	}
	if o.failOn != "ok" && o.failOn != "warn" && o.failOn != "fail" {
		return fmt.Errorf("invalid --fail-on: %s", o.failOn)
	}
	if name, value := firstNegativeDuration(o); value < 0 {
		return fmt.Errorf("invalid --%s: duration must not be negative", name)
	}
	return nil
}

// The documented check syntax places IDs before optional flags. The standard
// flag package stops parsing at the first positional argument, so move known
// flag/value pairs ahead of IDs while preserving their order.
func normalizeCheckArgs(args []string) []string {
	valueFlags := map[string]bool{
		"root": true, "output": true, "o": true, "db": true, "fail-on": true,
		"timeout": true, "interval": true, "max-age": true, "rescan": true,
		"refresh": true, "splay": true, "tag": true, "group": true, "match": true,
		"limit": true, "history": true, "since": true, "concurrency": true,
		"j": true, "config": true, "state-dir": true,
	}
	var flags, ids []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			ids = append(ids, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if valueFlags[name] && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, ids...)
}

func firstNegativeDuration(o options) (string, time.Duration) {
	values := []struct {
		name  string
		value time.Duration
	}{
		{"timeout", o.timeout}, {"interval", o.interval}, {"max-age", o.maxAge},
		{"since", o.since}, {"rescan", o.rescan}, {"refresh", o.refresh}, {"splay", o.splay},
	}
	for _, x := range values {
		if x.value < 0 {
			return x.name, x.value
		}
	}
	return "", 0
}

func queryDaemonPID(root string) int {
	sock, _ := daemon.Paths(root)
	c, e := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if e != nil {
		return 0
	}
	defer c.Close()
	hello, _ := json.Marshal(map[string]any{"client_version": version.Current, "proto_version": 1, "exe_mtime": executableMtime()})
	if e = rpc.Write(c, rpc.Request{ID: 0, Method: "hello", Params: hello}); e != nil {
		return 0
	}
	raw, e := rpc.Read(c)
	if e != nil {
		return 0
	}
	var resp struct {
		Result struct {
			PID int `json:"pid"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return 0
	}
	return resp.Result.PID
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Millisecond {
		return "<1ms"
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	s := strconv.FormatFloat(d.Seconds(), 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "s"
}

func rsStarted(rs []runner.Result, fallback time.Time) time.Time {
	started := fallback
	for _, r := range rs {
		if !r.Cached && !r.Start.IsZero() && r.Start.Before(started) {
			started = r.Start
		}
	}
	return started
}

func launcherEnv() []string {
	var out []string
	for _, x := range os.Environ() {
		i := strings.IndexByte(x, '=')
		if i < 1 {
			continue
		}
		k := x[:i]
		if k == "PATH" || k == "HOME" || k == "USER" || k == "LANG" || k == "TZ" || k == "TERM" || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "XDG_") {
			out = append(out, x)
		}
	}
	return out
}
func daemonLaunchArgs(o options) []string {
	p := []string{"daemon", "run", "--root", o.root}
	if o.followSymlinks {
		p = append(p, "--follow-symlinks")
	}
	if o.allowUnsafe {
		p = append(p, "--allow-unsafe")
	}
	if o.config != "" {
		p = append(p, "--config", o.config)
	}
	if o.rescan > 0 {
		p = append(p, "--rescan", o.rescan.String())
	}
	if o.stateDir != "" {
		p = append(p, "--state-dir", o.stateDir)
	}
	if o.noState {
		p = append(p, "--no-state")
	}
	if o.concurrency > 0 {
		p = append(p, "--concurrency", strconv.Itoa(o.concurrency))
	}
	if o.limit > 0 {
		p = append(p, "--history", strconv.Itoa(o.limit))
	}
	return p
}
func executableMtime() int64 {
	exe, e := os.Executable()
	if e != nil {
		return 0
	}
	st, e := os.Stat(exe)
	if e != nil {
		return 0
	}
	return st.ModTime().UnixNano()
}
