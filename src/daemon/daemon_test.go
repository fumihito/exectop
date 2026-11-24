package daemon

import (
	"context"
	"encoding/json"
	"exectop/src/coordinator"
	"exectop/src/rpc"
	"exectop/src/runner"
	"exectop/src/version"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunRPC(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "ok")
	if e := os.WriteFile(p, []byte("#!/bin/sh\necho hello\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.Start(); e != nil {
		if strings.Contains(e.Error(), "operation not permitted") {
			t.Skip(e)
		}
		t.Fatal(e)
	}
	defer d.Stop()
	logBytes, e := os.ReadFile(d.logPath)
	if e != nil || !strings.Contains(string(logBytes), `"msg":"daemon started"`) {
		t.Fatalf("daemon log=%q err=%v", logBytes, e)
	}
	c, e := net.Dial("unix", d.Socket)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	hello, _ := json.Marshal(map[string]any{"proto_version": 1})
	if e = rpc.Write(c, rpc.Request{ID: 0, Method: "hello", Params: hello}); e != nil {
		t.Fatal(e)
	}
	if _, e = rpc.Read(c); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(map[string]any{"force": true})
	if e = rpc.Write(c, rpc.Request{ID: 1, Method: "run", Params: b}); e != nil {
		t.Fatal(e)
	}
	raw, e := rpc.Read(c)
	if e != nil {
		t.Fatal(e)
	}
	var r rpc.Response
	if e = json.Unmarshal(raw, &r); e != nil || r.Error != nil {
		t.Fatal(e, r.Error)
	}
	if r.Result == nil {
		t.Fatal("empty result")
	}
}

func TestStopDrainsActiveClientRequests(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, release := d.beginRequest()
	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Stop returned before active request drained")
	case <-time.After(25 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish after active request drained")
	}
}

func TestDaemonLogRotatesAtTenMiB(t *testing.T) {
	d := &Daemon{logPath: filepath.Join(t.TempDir(), "daemon.log")}
	if e := d.openLog(); e != nil {
		t.Fatal(e)
	}
	d.writeLog("INFO", strings.Repeat("x", maxLogBytes), nil)
	d.writeLog("INFO", "after rotation", nil)
	d.closeLog()
	if _, e := os.Stat(d.logPath + ".1"); e != nil {
		t.Fatalf("rotated log missing: %v", e)
	}
	if b, e := os.ReadFile(d.logPath); e != nil || !strings.Contains(string(b), "after rotation") {
		t.Fatalf("current log=%q err=%v", b, e)
	}
}

func TestLockInfoIsDiagnosticJSON(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "lock")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	started := time.Unix(123, 456)
	if e = writeLockInfo(f, started); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(f.Name())
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil || v["mode"] != "daemon" || v["version"] != version.Current {
		t.Fatalf("lock info=%s err=%v", b, e)
	}
}

func TestRestartRestoresFreshResult(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "slow")
	if e := os.WriteFile(p, []byte("#!/bin/sh\necho restored\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "exectop.toml"), []byte("[defaults]\nmin_interval=1h\n"), 0600); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	v, e := d.coord.Request(context.Background(), "slow", coordinator.Freshness{Force: true}, runner.Trigger("request"))
	if e != nil || v.Result.Status == "" {
		t.Fatal(v, e)
	}
	d.Stop()
	d, e = New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	v, e = d.coord.Request(context.Background(), "slow", coordinator.Freshness{}, runner.Trigger("request"))
	if e != nil || !v.Cached || !v.Restored || !v.Result.Restored {
		t.Fatalf("restored=%+v err=%v", v, e)
	}
}

func TestRunWaitFalseReturnsIDsWithoutWaiting(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "async")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nsleep 0.05\n"), 0700); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	b, _ := json.Marshal(map[string]any{"wait": false, "force": true})
	start := time.Now()
	responses := d.handle(rpc.Request{ID: 9, Method: "run", Params: b})
	if time.Since(start) > 40*time.Millisecond || len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("responses=%+v elapsed=%v", responses, time.Since(start))
	}
	if _, ok := responses[0].Result.(map[string]any); !ok {
		t.Fatalf("result=%T", responses[0].Result)
	}
}

func TestRunStreamEmitsResultsAndFinal(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "ok"), []byte("#!/bin/sh\necho streamed\n"), 0700); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	b, _ := json.Marshal(map[string]any{"force": true, "wait": true})
	ch, cleanup, ok := d.stream(rpc.Request{ID: 4, Method: "run", Params: b})
	if !ok {
		t.Fatal("run was not streamed")
	}
	defer cleanup()
	var gotResult, gotFinal bool
	for v := range ch {
		if v.Final {
			gotFinal = true
			continue
		}
		if v.Error != nil {
			t.Fatal(v.Error)
		}
		if v.Result != nil {
			gotResult = true
		}
	}
	if !gotResult || !gotFinal {
		t.Fatalf("result=%v final=%v", gotResult, gotFinal)
	}
}

func TestDaemonStateDirectoryOptions(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "check"), []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	custom := filepath.Join(t.TempDir(), "states")
	d, e := NewWithOptions(root, Options{StateDir: custom})
	if e != nil {
		t.Fatal(e)
	}
	if got := d.stateDir("a/b"); got != filepath.Join(custom, "a%2Fb") {
		t.Fatalf("custom state=%q", got)
	}
	d.Stop()
	d, e = NewWithOptions(root, Options{NoState: true})
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	if got := d.stateDir("a/b"); got != "" {
		t.Fatalf("no state=%q", got)
	}
}

func TestInvalidGlobalConfigRegistersErrors(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "check"), []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "exectop.toml"), []byte("[global\n"), 0600); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	entry, ok := d.store.Get("check")
	if !ok || entry.Spec.InvalidReason == "" || entry.Latest != nil {
		t.Fatalf("entry=%+v ok=%v", entry, ok)
	}
	v, e := d.coord.Request(context.Background(), "check", coordinator.Freshness{Force: true}, runner.Trigger("request"))
	if e != nil || v.Result.Status != "ERROR" {
		t.Fatalf("result=%+v err=%v", v, e)
	}
}

func TestReloadAppliesDaemonBackgroundSetting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "check"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "exectop.toml")
	if err := os.WriteFile(config, []byte("[daemon]\nbackground=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stop()
	if d.scheduler.Running() {
		t.Fatal("scheduler unexpectedly running")
	}
	if err = os.WriteFile(config, []byte("[daemon]\nbackground=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	responses := d.handle(rpc.Request{ID: 21, Method: "reload"})
	if len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("reload=%+v", responses)
	}
	if !d.scheduler.Running() {
		t.Fatal("reload did not start background scheduler")
	}
	if err = os.WriteFile(config, []byte("[daemon]\nbackground=false\n[persist]\nprune_interval=2h\n[persist.retention]\nruns=1h\n"), 0600); err != nil {
		t.Fatal(err)
	}
	responses = d.handle(rpc.Request{ID: 22, Method: "reload"})
	if len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("second reload=%+v", responses)
	}
	if d.scheduler.Running() || d.pruneInterval != 2*time.Hour || d.retention.RunsAge != time.Hour {
		t.Fatalf("reloaded runtime config running=%v interval=%v retention=%+v", d.scheduler.Running(), d.pruneInterval, d.retention)
	}
	if err = os.WriteFile(config, []byte("[daemon]\nbackground=false\nidle_exit=1h\n"), 0600); err != nil {
		t.Fatal(err)
	}
	responses = d.handle(rpc.Request{ID: 23, Method: "reload"})
	if len(responses) != 1 || responses[0].Error != nil || !d.idleStarted.Load() {
		t.Fatalf("idle reload responses=%+v started=%v", responses, d.idleStarted.Load())
	}
	if err = os.WriteFile(config, []byte("[global]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	responses = d.handle(rpc.Request{ID: 24, Method: "reload"})
	d.stateMu.RLock()
	idleExit := d.idleExit
	d.stateMu.RUnlock()
	if len(responses) != 1 || responses[0].Error != nil || d.background || idleExit != 0 || d.pruneInterval != time.Hour || d.retention.RunsMax != 1000 {
		t.Fatalf("removed settings were retained: responses=%+v background=%v idle=%v prune=%v retention=%+v", responses, d.background, idleExit, d.pruneInterval, d.retention)
	}
}

func TestDefaultRetentionAndPartialRetentionConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "check"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if d.pruneInterval != time.Hour || d.retention.RunsMax != 1000 || d.retention.RunsAge != 168*time.Hour || d.retention.OutputMin != 20 {
		t.Fatalf("defaults interval=%v retention=%+v", d.pruneInterval, d.retention)
	}
	d.Stop()
	if err := os.WriteFile(filepath.Join(root, "exectop.toml"), []byte("[persist.retention]\nruns=1h\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err = New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stop()
	if d.retention.RunsAge != time.Hour || d.retention.RunsMax != 1000 || d.retention.OutputAge != 24*time.Hour {
		t.Fatalf("partial retention=%+v", d.retention)
	}
}

func TestReloadStartsPruneLoopWhenEnabled(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "check"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "exectop.toml")
	if err := os.WriteFile(config, []byte("[persist]\nprune_interval=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stop()
	if d.pruneStarted.Load() {
		t.Fatal("prune loop unexpectedly started")
	}
	if err = os.WriteFile(config, []byte("[persist]\nprune_interval=1h\n"), 0600); err != nil {
		t.Fatal(err)
	}
	responses := d.handle(rpc.Request{ID: 25, Method: "reload"})
	if len(responses) != 1 || responses[0].Error != nil || !d.pruneStarted.Load() {
		t.Fatalf("prune reload responses=%+v started=%v", responses, d.pruneStarted.Load())
	}
}

func TestRPCFilterSelectsChecks(t *testing.T) {
	root := t.TempDir()
	if e := os.Mkdir(filepath.Join(root, "ops"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "ops", "disk"), []byte("#!/bin/sh\n# exectop: tags=disk\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "plain"), []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	b, _ := json.Marshal(map[string]any{"filter": map[string]any{"tag": "disk"}})
	rs := d.handle(rpc.Request{ID: 10, Method: "list", Params: b})
	if len(rs) != 1 || rs[0].Error != nil {
		t.Fatalf("responses=%+v", rs)
	}
	v, ok := rs[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("result=%T", rs[0].Result)
	}
	checks, ok := v["checks"].([]any)
	if !ok || len(checks) != 1 {
		t.Fatalf("checks=%#v", v["checks"])
	}
	bad, _ := json.Marshal(map[string]any{"filter": map[string]any{"match": "["}})
	rs = d.handle(rpc.Request{ID: 11, Method: "list", Params: bad})
	if len(rs) != 1 || rs[0].Error == nil || rs[0].Error.Code != "invalid_filter" {
		t.Fatalf("invalid filter response=%+v", rs)
	}
}

func TestRescanDefersRemovalUntilRunCompletes(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "slow")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nsleep 0.08\n"), 0700); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	done := make(chan struct{})
	go func() {
		_, _ = d.coord.Request(context.Background(), "slow", coordinator.Freshness{Force: true}, runner.Trigger("request"))
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if e, ok := d.store.Get("slow"); ok && e.Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if e, ok := d.store.Get("slow"); !ok || !e.Running {
		t.Fatal("run did not start")
	}
	if e := os.Remove(p); e != nil {
		t.Fatal(e)
	}
	rs := d.handle(rpc.Request{ID: 12, Method: "rescan"})
	if len(rs) != 1 || rs[0].Error != nil {
		t.Fatalf("rescan=%+v", rs)
	}
	if _, ok := d.store.Get("slow"); !ok {
		t.Fatal("running check removed before completion")
	}
	<-done
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.store.Get("slow"); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("check was not removed after completion")
}

func TestRescanReportsAddedRemovedAndUpdated(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(root, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	write("old", "echo old")
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	if e = os.Remove(filepath.Join(root, "old")); e != nil {
		t.Fatal(e)
	}
	write("new", "echo new")
	write("old", "echo changed")
	r := d.handle(rpc.Request{ID: 13, Method: "rescan"})
	if len(r) != 1 || r[0].Error != nil {
		t.Fatalf("rescan=%+v", r)
	}
	v, ok := r[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("result=%T", r[0].Result)
	}
	if len(v["added"].([]string)) != 1 || v["added"].([]string)[0] != "new" || len(v["updated"].([]string)) != 1 || v["updated"].([]string)[0] != "old" {
		t.Fatalf("diff=%#v", v)
	}
	// A second rescan after removing the check reports its deferred-free state
	// as a removal and no longer reports it as updated.
	if e = os.Remove(filepath.Join(root, "old")); e != nil {
		t.Fatal(e)
	}
	r = d.handle(rpc.Request{ID: 14, Method: "rescan"})
	v = r[0].Result.(map[string]any)
	if len(v["removed"].([]string)) != 1 || v["removed"].([]string)[0] != "old" {
		t.Fatalf("removed=%#v", v)
	}
}

func TestRescanKeepsInvalidCheckAsError(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "check")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: timeout=1s\n"), 0700); e != nil {
		t.Fatal(e)
	}
	d, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Stop()
	if e := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: timeout=bad\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r := d.handle(rpc.Request{ID: 15, Method: "rescan"})
	if len(r) != 1 || r[0].Error != nil {
		t.Fatalf("rescan=%+v", r)
	}
	if e, ok := d.store.Get("check"); !ok || e.Spec.InvalidReason == "" {
		t.Fatalf("invalid check lost: %+v %v", e, ok)
	}
}
