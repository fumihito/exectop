package runner

import (
	"context"
	"encoding/json"
	"exectop/src/spec"
	"exectop/src/status"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLimitedAndClassificationHelpers(t *testing.T) {
	var b limited
	b.max = 4
	if n, e := b.Write([]byte("abcdef")); e != nil || n != 6 || b.b.String() != "abcd" || !b.truncated {
		t.Fatalf("limited=%+v n=%d err=%v", b, n, e)
	}
	if classify(nil, spec.Spec{OK: []int{0}}) != status.OK {
		t.Fatal("nil classification")
	}
	if n, e := b.Write([]byte("z")); e != nil || n != 1 || !b.truncated || b.b.String() != "abcd" {
		t.Fatalf("limited overflow=%+v n=%d err=%v", b, n, e)
	}
	if statusFrom(2, spec.Spec{Warn: []int{2}}) != status.Warn {
		t.Fatal("warn classification")
	}
	if statusFrom(3, spec.Spec{Unknown: []int{3}}) != status.Unknown {
		t.Fatal("unknown classification")
	}
	if signalName(syscall.SIGKILL) != "SIGKILL" || signalName(syscall.SIGTERM) != "SIGTERM" || signalName(syscall.SIGINT) != "SIGINT" || signalName(syscall.SIGUSR1) == "" {
		t.Fatal("signal names")
	}
	if !dangerous("DYLD_INSERT_LIBRARIES") || !dangerous("ENV") || dangerous("PATH") {
		t.Fatal("dangerous environment classification")
	}
}

func TestResultJSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	want := Result{ID: "x", Iter: 3, Trigger: "request", Status: status.Warn, ExitCode: 2, Signal: "", Reason: "r", Start: now, End: now.Add(25 * time.Millisecond), Duration: 25 * time.Millisecond, Summary: "summary", Stdout: []byte("out"), Stderr: []byte("err"), Truncated: true, Metrics: map[string]float64{"latency": 1.5}, SpecHash: "hash", Cached: true, Joined: true, Restored: true, Age: 2 * time.Second, NextEligible: now.Add(time.Minute)}
	b, e := json.Marshal(want)
	if e != nil {
		t.Fatal(e)
	}
	var got Result
	if e = json.Unmarshal(b, &got); e != nil {
		t.Fatal(e)
	}
	if got.ID != want.ID || got.Status != want.Status || string(got.Stdout) != "out" || got.Metrics["latency"] != 1.5 || !got.Cached || got.Age != want.Age {
		t.Fatalf("got=%+v json=%s", got, b)
	}
}

func TestResultJSONUsesNullForUnsetNextEligible(t *testing.T) {
	b, err := json.Marshal(Result{ID: "x", Status: status.OK, ExitCode: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"name":""`) || !strings.Contains(string(b), `"group":""`) || !strings.Contains(string(b), `"tags":[]`) || !strings.Contains(string(b), `"metrics":{}`) || !strings.Contains(string(b), `"next_eligible_at":null`) {
		t.Fatalf("json=%s", b)
	}
}

func TestEnvironmentFiltering(t *testing.T) {
	t.Setenv("EXECTOP_TEST_KEEP", "yes")
	t.Setenv("LD_PRELOAD", "bad")
	e := environment(spec.Spec{Env: []string{"CHECK_VALUE=ok", "BASH_ENV=bad"}}, Meta{Version: "v", Trigger: "request", Mode: "run", PrevStatus: status.Status("NONE")})
	joined := strings.Join(e, "\n")
	if !strings.Contains(joined, "CHECK_VALUE=ok") || strings.Contains(joined, "BASH_ENV") || strings.Contains(joined, "LD_PRELOAD") {
		t.Fatalf("env=%q", joined)
	}
	managed := strings.Join(environment(spec.Spec{Env: []string{"EXECTOP=bad", "EXECTOP_MODE=bad", "CUSTOM=yes"}}, Meta{Version: "v", Trigger: "request", Mode: "run", PrevStatus: status.Status("NONE")}), "\n")
	if strings.Contains(managed, "EXECTOP=bad") || strings.Contains(managed, "EXECTOP_MODE=bad") || !strings.Contains(managed, "EXECTOP=1") || !strings.Contains(managed, "EXECTOP_MODE=run") {
		t.Fatalf("managed environment was overridden: %q", managed)
	}
	if strings.Contains(strings.Join(environment(spec.Spec{}, Meta{Version: "v", InheritEnv: true}), "\n"), "LD_PRELOAD") {
		t.Fatal("dangerous inherited env")
	}
	if !strings.Contains(strings.Join(environment(spec.Spec{}, Meta{Version: "v"}), "\n"), "EXECTOP_PREV_TS=") {
		t.Fatal("initial previous timestamp must be present and empty")
	}
}

func TestRunStatusAndTimeout(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x.sh")
	if e := os.WriteFile(p, []byte("#!/bin/sh\necho hello\nexit 2\n"), 0700); e != nil {
		t.Fatal(e)
	}
	s := spec.Spec{ID: "x", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Millisecond, OK: []int{0}, Warn: []int{2}, Unknown: []int{3}, OutputLimit: 64}
	r := Run(context.Background(), s, Meta{Iter: 1})
	if r.Status != status.Warn || r.Summary != "hello" {
		t.Fatal(r.Status, r.Summary)
	}
	if e := os.WriteFile(p, []byte("#!/bin/sh\nsleep 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	s.Timeout = 10 * time.Millisecond
	r = Run(context.Background(), s, Meta{Iter: 2})
	if r.Status != status.Timeout {
		t.Fatal(r.Status, r.Reason)
	}
}

func TestRunExitCodeAndConfiguredEnvironment(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x.sh")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' \"$CHECK_VALUE\"\nexit 7\n"), 0700); e != nil {
		t.Fatal(e)
	}
	s := spec.Spec{ID: "x", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, Env: []string{"CHECK_VALUE=from-spec"}, OutputLimit: 64}
	r := Run(context.Background(), s, Meta{Iter: 1})
	if r.ExitCode != 7 || r.Status != status.Fail || r.Summary != "from-spec" {
		t.Fatalf("%+v", r)
	}
}

func TestRunStructuredOutputAndSignal(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "structured")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '{\"summary\":\"json summary\",\"metrics\":{\"value\":2.5}}'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r := Run(context.Background(), spec.Spec{ID: "structured", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, OutputLimit: 1024, OK: []int{0}}, Meta{Version: "test", Trigger: "manual", Mode: "run", PrevStatus: status.OK, PrevTS: time.Now().Add(-time.Minute)})
	if r.Status != status.OK || r.Summary != "json summary" || r.Metrics["value"] != 2.5 || r.Variables["owner"] != nil {
		t.Fatalf("structured=%+v", r)
	}
	if e := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s\\n' '{\"summary\":\"json summary\",\"metrics\":{\"value\":2.5},\"owner\":\"ops\",\"healthy\":true,\"count\":3}'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r = Run(context.Background(), spec.Spec{ID: "structured", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, OutputLimit: 1024, OK: []int{0}}, Meta{})
	if r.Variables["owner"] != "ops" || r.Variables["healthy"] != true || r.Variables["count"] != float64(3) {
		t.Fatalf("variables=%v", r.Variables)
	}
	b, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"variables"`) {
		t.Fatalf("json variables: %s %v", b, err)
	}
	if e := os.WriteFile(p, []byte("#!/bin/sh\nkill -TERM $$\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r = Run(context.Background(), spec.Spec{ID: "signal", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second}, Meta{})
	if r.Status != status.Fail || r.Signal != "SIGTERM" || r.Reason != "signal SIGTERM" {
		t.Fatalf("signal=%+v", r)
	}
}

func TestRunTruncatesOutputAndSetsContractEnv(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x.sh")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nprintf '123456789\\n'\nprintf '%s|%s|%s|%s|%s\\n' \"$EXECTOP\" \"$EXECTOP_MODE\" \"$EXECTOP_TRIGGER\" \"$EXECTOP_PREV_STATUS\" \"$EXECTOP_STATE_DIR\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	state := filepath.Join(d, "state")
	s := spec.Spec{ID: "x", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, OutputLimit: 4, OK: []int{0}}
	r := Run(context.Background(), s, Meta{Iter: 1, Mode: "run", Trigger: "request", StateDir: state})
	if !r.Truncated || len(r.Stdout) != 4 || r.Status != status.OK {
		t.Fatalf("%+v stdout=%q", r, r.Stdout)
	}
	if st, e := os.Stat(state); e != nil || st.Mode().Perm() != 0700 {
		t.Fatalf("state=%v %v", st, e)
	}
}

func TestRunParentCancellationIsError(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x.sh")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nsleep 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- Run(ctx, spec.Spec{ID: "x", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Millisecond}, Meta{Iter: 1})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	r := <-done
	if r.Status != status.Error || r.Reason != "cancelled" {
		t.Fatalf("%+v", r)
	}
}

func TestRunEarlyOutcomes(t *testing.T) {
	d := t.TempDir()
	missing := spec.Spec{ID: "missing", Path: filepath.Join(d, "nope")}
	if r := Run(context.Background(), missing, Meta{}); r.Status != status.Vanished {
		t.Fatalf("missing=%+v", r)
	}
	p := filepath.Join(d, "x")
	if e := os.WriteFile(p, []byte("not executable"), 0600); e != nil {
		t.Fatal(e)
	}
	if r := Run(context.Background(), spec.Spec{ID: "unsafe", Path: p}, Meta{}); r.Status != status.Error {
		t.Fatalf("unsafe=%+v", r)
	}
	parent := filepath.Join(d, "parent")
	if e := os.Mkdir(parent, 0700); e != nil {
		t.Fatal(e)
	}
	parentCheck := filepath.Join(parent, "check")
	if e := os.WriteFile(parentCheck, []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(parent, 0770); e != nil {
		t.Fatal(e)
	}
	if r := Run(context.Background(), spec.Spec{ID: "parent/check", Path: parentCheck, Cwd: parent, OK: []int{0}}, Meta{}); r.Status != status.Error {
		t.Fatalf("unsafe parent=%+v", r)
	}
	_ = os.Chmod(parent, 0700)
	if r := Run(context.Background(), spec.Spec{ID: "badcwd", Path: p, Cwd: filepath.Join(d, "gone"), OK: []int{0}}, Meta{}); r.Status != status.Error {
		t.Fatalf("badcwd=%+v", r)
	}
	execPath := filepath.Join(d, "exec")
	if e := os.WriteFile(execPath, []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if r := Run(context.Background(), spec.Spec{ID: "start-error", Path: execPath, Cwd: filepath.Join(d, "gone"), OK: []int{0}}, Meta{}); r.Status != status.Error {
		t.Fatalf("start error=%+v", r)
	}
	if r := Run(context.Background(), spec.Spec{ID: "invalid", Path: p, InvalidReason: "bad specification"}, Meta{}); r.Status != status.Error || r.Reason != "bad specification" {
		t.Fatalf("invalid=%+v", r)
	}
}

func TestRunMissingScriptIsVanished(t *testing.T) {
	r := Run(context.Background(), spec.Spec{ID: "gone", Path: filepath.Join(t.TempDir(), "gone")}, Meta{})
	if r.Status != status.Vanished || !strings.Contains(r.Reason, "vanished") {
		t.Fatalf("missing script=%+v", r)
	}
}

func TestCheckDependencies(t *testing.T) {
	if reason := CheckDependencies(spec.Spec{Requires: []string{"command-that-does-not-exist"}}); reason == "" {
		t.Fatal("missing command was accepted")
	}
	if reason := CheckDependencies(spec.Spec{RequiresOS: []string{"not-this-os"}}); reason == "" {
		t.Fatal("wrong OS was accepted")
	}
	if reason := CheckDependencies(spec.Spec{RequiresPath: []string{"/path/that/does/not/exist"}}); reason == "" {
		t.Fatal("missing path was accepted")
	}
}

func TestRunScriptRemovedDuringExecutionIsVanished(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "slow")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 0.15\n"), 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() {
		done <- Run(context.Background(), spec.Spec{ID: "slow", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, OK: []int{0}}, Meta{})
	}()
	time.Sleep(60 * time.Millisecond)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.Status != status.Vanished {
		t.Fatalf("removed script=%+v", r)
	}
}

func TestRunMetadataAndRunningPIDFile(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "meta")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s|%s|%s|%s|%s\\n' \"$EXECTOP_VERSION\" \"$EXECTOP_ITER\" \"$EXECTOP_PREV_STATUS\" \"$EXECTOP_PREV_TS\" \"$EXECTOP_STATE_DIR\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	pidFile := filepath.Join(d, "running")
	state := filepath.Join(d, "state")
	if e := os.Mkdir(state, 0777); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(state, 0777); e != nil {
		t.Fatal(e)
	}
	r := Run(context.Background(), spec.Spec{ID: "meta", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, Nice: 1, OutputLimit: 1024, OK: []int{0}}, Meta{Iter: 4, Trigger: "manual", Mode: "run", StateDir: state, RunningFile: pidFile, PrevStatus: status.Fail, PrevTS: time.Unix(123, 0)})
	if r.Status != status.OK || !strings.Contains(string(r.Stdout), "|4|FAIL|123|") {
		t.Fatalf("metadata=%+v stdout=%q", r, r.Stdout)
	}
	if info, e := os.Stat(state); e != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("state mode=%v err=%v", info.Mode().Perm(), e)
	}
	if b, e := os.ReadFile(pidFile); e != nil || len(b) != 0 {
		t.Fatalf("running file=%q err=%v", b, e)
	}
}

func TestRunDefaultsMissingMetadata(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "defaults")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nprintf '%s|%s|%s\\n' \"$EXECTOP_MODE\" \"$EXECTOP_TRIGGER\" \"$EXECTOP_PREV_STATUS\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r := Run(context.Background(), spec.Spec{ID: "defaults", Path: p, Cwd: d, Timeout: time.Second, Grace: time.Second, OutputLimit: 128, OK: []int{0}}, Meta{})
	if r.Status != status.OK || r.Trigger != "manual" || string(r.Stdout) != "run|manual|NONE\n" {
		t.Fatalf("defaults=%+v stdout=%q", r, r.Stdout)
	}
}
