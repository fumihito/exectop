package render

import (
	"exectop/src/runner"
	"exectop/src/status"
	"strings"
	"testing"
	"time"
)

func TestTableUsesStableDurationAndFlags(t *testing.T) {
	out := Table([]runner.Result{
		{ID: "x", Name: "unsafe\nname", Status: status.OK, Duration: 5 * time.Millisecond, ExitCode: 0},
		{ID: "y", Status: status.Fail, Duration: 1500 * time.Millisecond, ExitCode: 1, Cached: true, Age: 4 * time.Minute},
	})
	if !strings.Contains(out, "SUMMARY  PATH") || !strings.Contains(out, "  x\n") {
		t.Fatalf("path column missing: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.Index(lines[0], "LAST")
	if last < 0 || strings.Index(lines[1], "now") != last || strings.Index(lines[2], "4m0s ago") != last {
		t.Fatalf("LAST is not aligned: %q", out)
	}
	if !strings.Contains(out, "5ms") || !strings.Contains(out, "1.5s") {
		t.Fatalf("durations missing: %q", out)
	}
}

func TestTableUsesShortNameAndPathColumns(t *testing.T) {
	name := "  これは20文字を超える長い名前ですさらに長い名前  "
	wantName := string([]rune(strings.TrimSpace(name))[:20])
	out := Table([]runner.Result{{ID: "examples/check.sh", Name: name, Status: status.OK}})
	if !strings.Contains(out, "ST  NAME") || !strings.Contains(out, "SUMMARY  PATH") {
		t.Fatalf("header=%q", out)
	}
	if !strings.Contains(out, wantName) {
		t.Fatalf("short name was not trimmed: %q", out)
	}
	if !strings.Contains(out, "examples/check.sh\n") {
		t.Fatalf("path was not emitted: %q", out)
	}
}
