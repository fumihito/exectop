package bench

import (
	"encoding/json"
	"exectop/src/spec"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompareFilesUsesMedianAndRejectsDifferentWorkloads(t *testing.T) {
	d := t.TempDir()
	base := filepath.Join(d, "base.json")
	cur := filepath.Join(d, "cur.json")
	write := func(path string, value any) {
		b, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(base, map[string]any{"params": map[string]any{"checks": 10}, "scenarios": map[string]any{"discover": map[string]any{"median": map[string]any{"discover_ms": 100.0}}}})
	write(cur, map[string]any{"params": map[string]any{"checks": 10}, "scenarios": map[string]any{"discover": map[string]any{"median": map[string]any{"discover_ms": 111.0}}}})
	bad, e := CompareFiles(base, cur, 10)
	if e != nil || len(bad) != 1 || bad[0] != "discover.discover_ms" {
		t.Fatalf("bad=%v err=%v", bad, e)
	}
	write(cur, map[string]any{"params": map[string]any{"checks": 11}, "scenarios": map[string]any{"discover": map[string]any{"discover_ms": 1.0}}})
	if _, e = CompareFiles(base, cur, 10); e == nil {
		t.Fatal("different workload parameters were accepted")
	}
}

func TestRunRepeatReportIsJSONSerializable(t *testing.T) {
	d := t.TempDir()
	check := filepath.Join(d, "check.sh")
	if e := os.WriteFile(check, []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	r, e := Run(RunOptions{Root: d, Scenarios: []string{"discover"}, Repeat: 2})
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(r)
	if e != nil || len(b) == 0 {
		t.Fatalf("repeat report is not JSON serializable: bytes=%d err=%v", len(b), e)
	}
	runs, ok := r.Scenarios["discover"]["runs"].([]any)
	if !ok || len(runs) != 2 {
		t.Fatalf("runs=%T %v", r.Scenarios["discover"]["runs"], r.Scenarios["discover"]["runs"])
	}
}

func TestEvaluateGatesUsesMedianWhenPresent(t *testing.T) {
	r := RunReport{Checks: 100, Pass: true, Scenarios: map[string]map[string]any{
		"run-cold": {"parallel_efficiency": 0.1, "median": map[string]any{"parallel_efficiency": 0.8}},
	}}
	evaluateGates(&r)
	for _, failure := range r.Failures {
		if failure == "run-cold" {
			t.Fatalf("first sample incorrectly used: %+v", r)
		}
	}
}

func TestEvaluateGatesSkipsNonApplicableOverhead(t *testing.T) {
	r := RunReport{Pass: true, Scenarios: map[string]map[string]any{
		"overhead": {"skipped": true, "overhead_us": 999999},
	}}
	evaluateGates(&r)
	if !r.Pass || len(r.Failures) != 0 {
		t.Fatalf("skipped overhead failed: %+v", r)
	}
}

func TestEvaluateGatesSkipsCachedGateWhenNotAllCached(t *testing.T) {
	r := RunReport{Checks: 2, Pass: true, Scenarios: map[string]map[string]any{
		"run-cached": {"wall_ms": 999, "cached_count": 0, "all_cached": false},
	}}
	evaluateGates(&r)
	if !r.Pass {
		t.Fatalf("non-cached workload incorrectly failed cached gate: %+v", r)
	}
}

func TestGenerate(t *testing.T) {
	d := t.TempDir()
	if e := Generate(GenOptions{Out: d, Checks: 4, Mean: time.Millisecond, OutputBytes: 10, Dist: "fixed"}); e != nil {
		t.Fatal(e)
	}
	matches, _ := filepath.Glob(filepath.Join(d, "g*", "check-*.sh"))
	if len(matches) != 4 {
		t.Fatalf("%d", len(matches))
	}
	if st, e := os.Stat(matches[0]); e != nil || st.Mode().Perm()&0111 == 0 {
		t.Fatal(e)
	}
	body, e := os.ReadFile(matches[0])
	if e != nil {
		t.Fatal(e)
	}
	if string(body) == "" || !contains(string(body), "sleep 0.001000000") {
		t.Fatalf("generated shell sleep is not fractional seconds: %s", body)
	}
}

func contains(s, want string) bool {
	return len(s) >= len(want) && strings.Contains(s, want)
}

func TestParseBytesBinarySuffixes(t *testing.T) {
	for in, want := range map[string]int{"64KiB": 64 * 1024, "2MiB": 2 * 1024 * 1024, "1K": 1024} {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", in, got, err)
		}
	}
}

func TestGenerateBinUsesDirectHelperLinks(t *testing.T) {
	d := t.TempDir()
	helper, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	if e = Generate(GenOptions{Out: d, Checks: 2, Mean: time.Millisecond, OutputBytes: 10, Kind: "bin", Helper: helper}); e != nil {
		t.Fatal(e)
	}
	matches, _ := filepath.Glob(filepath.Join(d, "g*", "benchcheck-*-*-*-*"))
	if len(matches) != 2 {
		t.Fatalf("bin checks=%v", matches)
	}
	if got := benchCheckDuration(matches[0]); got <= 0 {
		t.Fatalf("duration=%v", got)
	}
	c, err := spec.LoadDirectoryConfig(filepath.Join(d, "g000", ".exectop.toml"))
	if err != nil || len(c.Checks) != 2 {
		t.Fatalf("bin config=%+v err=%v", c, err)
	}
}

func TestGenerateZeroMeanKeepsPositiveTimeout(t *testing.T) {
	d := t.TempDir()
	if e := Generate(GenOptions{Out: d, Checks: 1, Mean: 0, Dist: "fixed"}); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(d, "g000", "check-00000.sh"))
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "timeout=1s") {
		t.Fatalf("zero mean generated zero timeout: %s", b)
	}
}

func TestGenerateIntervalRangeIsDeterministicAndBounded(t *testing.T) {
	d := t.TempDir()
	if e := Generate(GenOptions{Out: d, Checks: 8, Mean: time.Millisecond, Seed: 7, Dist: "fixed", IntervalMin: 5 * time.Second, IntervalMax: 60 * time.Second}); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 8; i++ {
		path := filepath.Join(d, "g"+formatGroup(i%2), "check-"+formatCheck(i)+".sh")
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		text := string(b)
		if !strings.Contains(text, "interval=") {
			t.Fatalf("missing interval in %s", path)
		}
		if !strings.Contains(text, "s timeout=") {
			t.Fatalf("interval is not a duration in %s: %s", path, text)
		}
		m, e := spec.ParseHeader(path)
		if e != nil {
			t.Fatal(e)
		}
		interval, e := time.ParseDuration(m["interval"])
		if e != nil || interval < 5*time.Second || interval > 60*time.Second {
			t.Fatalf("interval=%q parsed=%s err=%v", m["interval"], interval, e)
		}
	}
}

func formatGroup(n int) string { return padNumber(n, 3) }
func formatCheck(n int) string { return padNumber(n, 5) }
func padNumber(n, width int) string {
	s := strconv.Itoa(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}
