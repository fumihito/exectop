package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"exectop/src/internal/fault"
	"exectop/src/metrics"
	"exectop/src/sanitize"
	"exectop/src/spec"
	"exectop/src/status"
	"exectop/src/version"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Trigger string
type Meta struct {
	Trigger     Trigger
	Mode        string
	Iter        int
	PrevStatus  status.Status
	PrevTS      time.Time
	Version     string
	StateDir    string
	RunningFile string
	InheritEnv  bool
}
type Result struct {
	ID           string             `json:"id"`
	Name         string             `json:"name,omitempty"`
	Runbook      string             `json:"runbook,omitempty"`
	Group        string             `json:"group,omitempty"`
	Tags         []string           `json:"tags,omitempty"`
	Iter         int                `json:"iter"`
	Trigger      Trigger            `json:"trigger"`
	Status       status.Status      `json:"status"`
	ExitCode     int                `json:"exit_code"`
	Signal       string             `json:"signal"`
	Reason       string             `json:"reason"`
	Start        time.Time          `json:"start"`
	End          time.Time          `json:"end"`
	Duration     time.Duration      `json:"-"`
	Summary      string             `json:"summary"`
	Stdout       []byte             `json:"-"`
	Stderr       []byte             `json:"-"`
	Truncated    bool               `json:"truncated"`
	Metrics      map[string]float64 `json:"metrics,omitempty"`
	Variables    map[string]any     `json:"variables,omitempty"`
	SpecHash     string             `json:"spec_hash"`
	Cached       bool               `json:"cached"`
	Joined       bool               `json:"joined"`
	Restored     bool               `json:"restored"`
	Age          time.Duration      `json:"age_ms"`
	NextEligible time.Time          `json:"next_eligible_at"`
	Changed      bool               `json:"changed,omitempty"`
	ChangedAt    time.Time          `json:"changed_at,omitempty"`
}

func (r Result) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID           string             `json:"id"`
		Name         string             `json:"name"`
		Runbook      string             `json:"runbook"`
		Group        string             `json:"group"`
		Tags         []string           `json:"tags"`
		Iter         int                `json:"iter"`
		Trigger      Trigger            `json:"trigger"`
		Status       status.Status      `json:"status"`
		ExitCode     int                `json:"exit_code"`
		Signal       string             `json:"signal"`
		Reason       string             `json:"reason"`
		Start        time.Time          `json:"start"`
		End          time.Time          `json:"end"`
		DurationMS   int64              `json:"duration_ms"`
		Summary      string             `json:"summary"`
		Stdout       string             `json:"stdout"`
		Stderr       string             `json:"stderr"`
		Truncated    bool               `json:"truncated"`
		Metrics      map[string]float64 `json:"metrics"`
		Variables    map[string]any     `json:"variables"`
		SpecHash     string             `json:"spec_hash"`
		Cached       bool               `json:"cached"`
		Joined       bool               `json:"joined"`
		Restored     bool               `json:"restored"`
		AgeMS        int64              `json:"age_ms"`
		NextEligible any                `json:"next_eligible_at"`
		Changed      bool               `json:"changed"`
		ChangedAt    time.Time          `json:"changed_at"`
	}
	var nextEligible any
	if !r.NextEligible.IsZero() {
		nextEligible = r.NextEligible
	}
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	metricsMap := r.Metrics
	if metricsMap == nil {
		metricsMap = map[string]float64{}
	}
	return json.Marshal(wire{ID: r.ID, Name: r.Name, Runbook: r.Runbook, Group: r.Group, Tags: tags, Iter: r.Iter, Trigger: r.Trigger, Status: r.Status, ExitCode: r.ExitCode, Signal: r.Signal, Reason: r.Reason, Start: r.Start, End: r.End, DurationMS: r.Duration.Milliseconds(), Summary: r.Summary, Stdout: string(r.Stdout), Stderr: string(r.Stderr), Truncated: r.Truncated, Metrics: metricsMap, Variables: r.Variables, SpecHash: r.SpecHash, Cached: r.Cached, Joined: r.Joined, Restored: r.Restored, AgeMS: r.Age.Milliseconds(), NextEligible: nextEligible, Changed: r.Changed, ChangedAt: r.ChangedAt})
}
func (r *Result) UnmarshalJSON(b []byte) error {
	var v struct {
		ID                       string        `json:"id"`
		Name                     string        `json:"name"`
		Runbook                  string        `json:"runbook"`
		Group                    string        `json:"group"`
		Tags                     []string      `json:"tags"`
		Iter                     int           `json:"iter"`
		Trigger                  Trigger       `json:"trigger"`
		Status                   status.Status `json:"status"`
		ExitCode                 int           `json:"exit_code"`
		Signal, Reason           string
		Start, End               time.Time
		DurationMS               int64 `json:"duration_ms"`
		Summary, Stdout, Stderr  string
		Truncated                bool
		Metrics                  map[string]float64
		Variables                map[string]any
		SpecHash                 string `json:"spec_hash"`
		Cached, Joined, Restored bool
		AgeMS                    int64     `json:"age_ms"`
		NextEligible             time.Time `json:"next_eligible_at"`
		Changed                  bool      `json:"changed"`
		ChangedAt                time.Time `json:"changed_at"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	r.ID = v.ID
	r.Name = v.Name
	r.Runbook = v.Runbook
	r.Group = v.Group
	r.Tags = v.Tags
	r.Iter = v.Iter
	r.Trigger = v.Trigger
	r.Status = v.Status
	r.ExitCode = v.ExitCode
	r.Signal = v.Signal
	r.Reason = v.Reason
	r.Start = v.Start
	r.End = v.End
	r.Duration = time.Duration(v.DurationMS) * time.Millisecond
	r.Summary = v.Summary
	r.Stdout = []byte(v.Stdout)
	r.Stderr = []byte(v.Stderr)
	r.Truncated = v.Truncated
	r.Metrics = v.Metrics
	r.Variables = v.Variables
	r.SpecHash = v.SpecHash
	r.Cached = v.Cached
	r.Joined = v.Joined
	r.Restored = v.Restored
	r.Age = time.Duration(v.AgeMS) * time.Millisecond
	r.NextEligible = v.NextEligible
	r.Changed = v.Changed
	r.ChangedAt = v.ChangedAt
	return nil
}

type limited struct {
	b         bytes.Buffer
	n         int64
	max       int64
	truncated bool
}

func (w *limited) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	if w.max <= int64(w.b.Len()) {
		w.truncated = true
		return len(p), nil
	}
	take := len(p)
	if int64(w.b.Len()+take) > w.max {
		take = int(w.max) - w.b.Len()
		w.truncated = true
	}
	if take > 0 {
		w.b.Write(p[:take])
	}
	return len(p), nil
}
func Run(ctx context.Context, s spec.Spec, m Meta) Result {
	start := time.Now()
	defer func() { metrics.Observe("runner.duration", time.Since(start)) }()
	if m.Trigger == "" {
		m.Trigger = "manual"
	}
	if m.Mode == "" {
		m.Mode = "run"
	}
	if m.PrevStatus == "" {
		m.PrevStatus = status.Status("NONE")
	}
	group := ""
	if i := strings.IndexByte(s.ID, '/'); i >= 0 {
		group = s.ID[:i]
	}
	r := Result{ID: s.ID, Name: s.Name, Runbook: s.Runbook, Group: group, Tags: append([]string(nil), s.Tags...), Iter: m.Iter, Trigger: m.Trigger, Start: start, ExitCode: -1, SpecHash: s.Hash}
	if fault.Hit("runner.start_error") {
		r.Status = status.Error
		r.Reason = "fault injection: runner.start_error"
		r.End = time.Now()
		r.Duration = r.End.Sub(start)
		return r
	}
	if s.InvalidReason != "" {
		r.Status = status.Error
		r.Reason = s.InvalidReason
		r.End = time.Now()
		r.Duration = r.End.Sub(start)
		return r
	}
	st, statErr := os.Stat(s.Path)
	if statErr != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 || (!s.AllowUnsafe && st.Mode().Perm()&0022 != 0) || (!s.AllowUnsafe && !ownedByUserOrRoot(st)) || (!s.AllowUnsafe && unsafeParents(s)) {
		r.Status = status.Error
		if statErr != nil {
			r.Reason = statErr.Error()
			if os.IsNotExist(statErr) {
				r.Status = status.Vanished
				r.Reason = "check script vanished: " + statErr.Error()
			}
		} else {
			r.Reason = "check changed or is unsafe before execution"
		}
		r.End = time.Now()
		r.Duration = r.End.Sub(start)
		return r
	}
	if reason := CheckDependencies(s); reason != "" {
		r.Status = status.Error
		r.Reason = reason
		r.End = time.Now()
		r.Duration = r.End.Sub(start)
		return r
	}
	if m.StateDir != "" {
		if e := os.MkdirAll(m.StateDir, 0700); e != nil {
			r.Status = status.Error
			r.Reason = e.Error()
			r.End = time.Now()
			r.Duration = r.End.Sub(start)
			return r
		}
		if e := os.Chmod(m.StateDir, 0700); e != nil {
			r.Status = status.Error
			r.Reason = e.Error()
			r.End = time.Now()
			r.Duration = r.End.Sub(start)
			return r
		}
	}
	if m.Version == "" {
		m.Version = version.Current
	}
	runctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runctx, s.Path, s.Args...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = s.Grace
	cmd.Dir = s.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil
	out := &limited{max: s.OutputLimit}
	errout := &limited{max: s.OutputLimit}
	cmd.Stdout = out
	cmd.Stderr = errout
	cmd.Env = environment(s, m)
	if err := cmd.Start(); err != nil {
		r.Status = status.Error
		r.Reason = err.Error()
		if os.IsNotExist(err) {
			r.Status = status.Vanished
			r.Reason = "check script vanished: " + err.Error()
		}
		r.End = time.Now()
		r.Duration = r.End.Sub(start)
		return r
	}
	pid := cmd.Process.Pid
	if m.RunningFile != "" {
		addRunningPID(m.RunningFile, pid)
		defer removeRunningPID(m.RunningFile, pid)
	}
	vanished := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, e := os.Stat(s.Path); os.IsNotExist(e) {
					select {
					case vanished <- struct{}{}:
					default:
					}
					return
				}
			case <-watchDone:
				return
			}
		}
	}()
	if s.Nice != 0 {
		_ = syscall.Setpriority(syscall.PRIO_PGRP, pid, s.Nice)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-runctx.Done():
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		timer := time.NewTimer(s.Grace)
		select {
		case err = <-done:
		case <-timer.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			err = <-done
		}
		timer.Stop()
		if ctx.Err() != nil && ctx.Err() != context.DeadlineExceeded {
			r.Status = status.Error
			r.Reason = "cancelled"
		} else {
			r.Status = status.Timeout
			r.Reason = "timeout"
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	close(watchDone)
	wasVanished := false
	select {
	case <-vanished:
		wasVanished = true
	default:
	}
	r.End = time.Now()
	r.Duration = r.End.Sub(start)
	r.Stdout = out.b.Bytes()
	r.Stderr = errout.b.Bytes()
	r.Truncated = out.truncated || errout.truncated
	if err == nil {
		r.ExitCode = 0
	} else if x, ok := err.(*exec.ExitError); ok {
		r.ExitCode = x.ExitCode()
	}
	if r.Status == "" {
		r.Status = classify(err, s)
		if x, ok := err.(*exec.ExitError); ok {
			if ws, ok := x.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				r.Signal = signalName(ws.Signal())
				r.Reason = "signal " + r.Signal
			}
		}
	}
	if wasVanished && r.Status != status.Timeout && r.Reason != "cancelled" {
		r.Status = status.Vanished
		r.Reason = "check script vanished during execution"
	}
	r.Summary = sanitize.FirstLine(r.Stdout)
	r.ParseVariables()
	return r
}

// CheckDependencies validates declarations made in a check header. It is
// deliberately read-only and is shared by runner execution and `exectop
// validate`, so an agent can test a check without running it.
func CheckDependencies(s spec.Spec) string {
	for _, want := range s.RequiresOS {
		if want != "" && want != runtime.GOOS {
			return fmt.Sprintf("required OS %q is not available (current: %s)", want, runtime.GOOS)
		}
	}
	for _, command := range s.Requires {
		if command == "" {
			continue
		}
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Sprintf("required command %q is not available: %v", command, err)
		}
	}
	for _, path := range s.RequiresPath {
		if path == "" {
			continue
		}
		checkPath := path
		if !filepath.IsAbs(checkPath) {
			checkPath = filepath.Join(s.Cwd, checkPath)
		}
		if _, err := os.Stat(checkPath); err != nil {
			return fmt.Sprintf("required path %q is not available: %v", path, err)
		}
	}
	return ""
}

// ParseVariables decodes the first stdout line when it is a JSON object.
// It is also used when restoring results from persistent storage.
func (r *Result) ParseVariables() {
	line := strings.TrimSpace(sanitize.FirstLine(r.Stdout))
	if !strings.HasPrefix(line, "{") {
		return
	}
	var vars map[string]any
	if json.Unmarshal([]byte(line), &vars) != nil {
		return
	}
	r.Variables = vars
	if summary, ok := vars["summary"].(string); ok && summary != "" {
		r.Summary = sanitize.Text([]byte(summary))
	}
	if raw, ok := vars["metrics"].(map[string]any); ok {
		metrics := map[string]float64{}
		for key, value := range raw {
			if n, ok := value.(float64); ok {
				metrics[key] = n
			}
		}
		r.Metrics = metrics
	}
}

var runningMu sync.Mutex

func addRunningPID(path string, pid int) {
	runningMu.Lock()
	defer runningMu.Unlock()
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if f != nil {
		_, _ = fmt.Fprintln(f, pid)
		_ = f.Close()
	}
}
func removeRunningPID(path string, pid int) {
	runningMu.Lock()
	defer runningMu.Unlock()
	b, e := os.ReadFile(path)
	if e != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var out []string
	for _, x := range lines {
		if x != "" && x != strconv.Itoa(pid) {
			out = append(out, x)
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(out, "\n")+func() string {
		if len(out) > 0 {
			return "\n"
		}
		return ""
	}()), 0600)
}
func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	}
	return "SIG" + strings.ToUpper(s.String())
}
func classify(err error, s spec.Spec) status.Status {
	if err == nil {
		return statusFrom(0, s)
	}
	if x, ok := err.(*exec.ExitError); ok {
		if ws, ok := x.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return status.Fail
		}
		return statusFrom(x.ExitCode(), s)
	}
	return status.Error
}
func statusFrom(n int, s spec.Spec) status.Status {
	for _, x := range s.OK {
		if n == x {
			return status.OK
		}
	}
	for _, x := range s.Warn {
		if n == x {
			return status.Warn
		}
	}
	for _, x := range s.Unknown {
		if n == x {
			return status.Unknown
		}
	}
	return status.Fail
}
func environment(s spec.Spec, m Meta) []string {
	var e []string
	if m.InheritEnv {
		for _, x := range os.Environ() {
			k := x[:strings.IndexByte(x, '=')]
			if dangerous(k) {
				continue
			}
			e = append(e, x)
		}
	} else {
		for _, k := range []string{"PATH", "HOME", "USER", "LANG", "TZ", "TERM"} {
			if v, ok := os.LookupEnv(k); ok {
				e = append(e, k+"="+v)
			}
		}
		for _, x := range os.Environ() {
			k := x[:strings.IndexByte(x, '=')]
			if strings.HasPrefix(k, "LC_") {
				e = append(e, x)
			}
		}
	}
	for _, x := range s.Env {
		if i := strings.IndexByte(x, '='); i > 0 && !dangerous(x[:i]) && !managedEnvKey(x[:i]) {
			e = append(e, x)
		}
	}
	e = append(e, "EXECTOP=1", "EXECTOP_VERSION="+m.Version, "EXECTOP_CHECK_ID="+s.ID, "EXECTOP_MODE="+m.Mode, "EXECTOP_TRIGGER="+string(m.Trigger), "EXECTOP_ITER="+strconv.Itoa(m.Iter), "EXECTOP_TIMEOUT="+strconv.Itoa(int(s.Timeout.Seconds())), "EXECTOP_PREV_STATUS="+string(m.PrevStatus))
	prevTS := ""
	if !m.PrevTS.IsZero() {
		prevTS = strconv.FormatInt(m.PrevTS.Unix(), 10)
	}
	e = append(e, "EXECTOP_PREV_TS="+prevTS)
	if m.StateDir != "" {
		e = append(e, "EXECTOP_STATE_DIR="+m.StateDir)
	}
	return e
}
func managedEnvKey(k string) bool {
	switch k {
	case "EXECTOP", "EXECTOP_VERSION", "EXECTOP_CHECK_ID", "EXECTOP_MODE", "EXECTOP_TRIGGER", "EXECTOP_ITER", "EXECTOP_TIMEOUT", "EXECTOP_PREV_STATUS", "EXECTOP_PREV_TS", "EXECTOP_STATE_DIR":
		return true
	default:
		return false
	}
}
func ownedByUserOrRoot(st os.FileInfo) bool {
	x, ok := st.Sys().(*syscall.Stat_t)
	return !ok || int(x.Uid) == os.Getuid() || x.Uid == 0
}

func unsafeParents(s spec.Spec) bool {
	if s.ID == "" {
		return false
	}
	root := strings.TrimSuffix(s.Path, string(os.PathSeparator)+filepath.FromSlash(s.ID))
	if root == s.Path || root == "" {
		return false
	}
	for dir := filepath.Dir(s.Path); ; dir = filepath.Dir(dir) {
		st, e := os.Stat(dir)
		if e != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return true
		}
		if dir == root {
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return true
		}
	}
}
func dangerous(k string) bool {
	if strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "DYLD_") {
		return true
	}
	switch k {
	case "LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "ENV":
		return true
	}
	return false
}
