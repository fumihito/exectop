package main

import (
	"exectop/src/internal/examples"
	"exectop/src/runner"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallExamplesCopiesBundledFiles(t *testing.T) {
	if !examples.Available() {
		t.Skip("examples are populated by the build script")
	}
	root := t.TempDir()
	target := filepath.Join(root, "new-root", "exectop.d")
	if err := examples.Install(target); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target, "examples")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	for _, name := range []string{"README.md", "AGENTS.md", "check-template.txt"} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			t.Fatalf("bundled authoring file %s is missing: %v", name, err)
		}
	}
}

func TestFoundRegistersInvalidGlobalConfig(t *testing.T) {
	root := t.TempDir()
	check := filepath.Join(root, "check")
	if err := os.WriteFile(check, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "exectop.toml"), []byte("[global\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, specs := found(options{root: root})
	if len(specs) != 1 || specs[0].InvalidReason == "" {
		t.Fatalf("specs=%+v", specs)
	}
}

func TestDefaultRootPrefersExectopDirectory(t *testing.T) {
	dir := t.TempDir()
	testChdir(t, dir)
	if got := defaultRoot(); got != "./checks" {
		t.Fatalf("without exectop.d: root=%q, want ./checks", got)
	}
	if err := os.Mkdir(filepath.Join(dir, "exectop.d"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := defaultRoot(); got != "./exectop.d" {
		t.Fatalf("with exectop.d: root=%q, want ./exectop.d", got)
	}
}

func TestDefaultRootIgnoresExectopFile(t *testing.T) {
	dir := t.TempDir()
	testChdir(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "exectop.d"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := defaultRoot(); got != "./checks" {
		t.Fatalf("file named exectop.d: root=%q, want ./checks", got)
	}
}

// testChdir keeps the tests compatible with the module's Go 1.22 baseline.
func testChdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

func TestDefaultCommandArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "no arguments", args: []string{"exectop"}, want: []string{"exectop", "watch"}},
		{name: "flags only", args: []string{"exectop", "--output", "json"}, want: []string{"exectop", "watch", "--output", "json"}},
		{name: "explicit run", args: []string{"exectop", "run", "--no-daemon"}, want: []string{"exectop", "run", "--no-daemon"}},
		{name: "explicit oneshot", args: []string{"exectop", "oneshot", "--no-daemon"}, want: []string{"exectop", "oneshot", "--no-daemon"}},
		{name: "explicit list", args: []string{"exectop", "list"}, want: []string{"exectop", "list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultCommandArgs(tt.args)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("args=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeArchiveArgs(t *testing.T) {
	got := normalizeArchiveArgs([]string{"create", "bundle.tar.gz", "--root", "./exectop.d", "--mode=merge"}, "")
	want := []string{"--root", "./exectop.d", "--mode=merge", "create", "bundle.tar.gz"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args=%v, want %v", got, want)
	}
	got = normalizeArchiveArgs([]string{"bundle.tar.gz", "--mode", "replace"}, "extract")
	want = []string{"--mode", "replace", "extract", "bundle.tar.gz"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("implicit extract args=%v, want %v", got, want)
	}
}

func TestNormalizeCommand(t *testing.T) {
	if got := normalizeCommand("top"); got != "watch" {
		t.Fatalf("normalizeCommand(top)=%q, want watch", got)
	}
	for _, command := range []string{"watch", "oneshot", "run", "list", "check"} {
		if got := normalizeCommand(command); got != command {
			t.Fatalf("normalizeCommand(%q)=%q, want unchanged", command, got)
		}
	}
}

func TestRSStartedIgnoresCachedExecutionTimestamp(t *testing.T) {
	fallback := time.Now()
	old := runner.Result{Cached: true, Start: fallback.Add(-time.Hour)}
	if got := rsStarted([]runner.Result{old}, fallback); !got.Equal(fallback) {
		t.Fatalf("started=%v, want fallback %v", got, fallback)
	}
}

func TestValidOutput(t *testing.T) {
	for _, format := range []string{"", "table", "json", "jsonl"} {
		if !validOutput(format) {
			t.Fatalf("valid output rejected: %q", format)
		}
	}
	if validOutput("yaml") {
		t.Fatal("unsupported output accepted")
	}
}

func TestColorizeTableUsesMarkersAndHighlightsFailures(t *testing.T) {
	out := colorizeTable("ST  NAME\nOK  healthy\nFAIL broken\n")
	if !strings.Contains(out, "\033[1mST  NAME\033[0m") {
		t.Fatalf("header was not emphasized: %q", out)
	}
	if !strings.Contains(out, "\033[32m+ OK  healthy\033[0m") {
		t.Fatalf("OK marker/color missing: %q", out)
	}
	if !strings.Contains(out, "\033[1;7;31mX FAIL broken\033[0m") {
		t.Fatalf("failure emphasis missing: %q", out)
	}
}

func TestValidateOptions(t *testing.T) {
	if err := validateOptions(options{output: "yaml"}); err == nil {
		t.Fatal("invalid output accepted")
	}
	if err := validateOptions(options{failOn: "error"}); err == nil {
		t.Fatal("invalid fail-on accepted")
	}
	if err := validateOptions(options{interval: -time.Second}); err == nil {
		t.Fatal("negative interval accepted")
	}
	if err := validateOptions(options{output: "json", failOn: "fail", interval: time.Second}); err != nil {
		t.Fatal(err)
	}
}

func TestFirstNegativeDuration(t *testing.T) {
	if name, value := firstNegativeDuration(options{timeout: -time.Second}); name != "timeout" || value >= 0 {
		t.Fatalf("negative timeout=%q %v", name, value)
	}
	if name, value := firstNegativeDuration(options{root: "/tmp"}); name != "" || value != 0 {
		t.Fatalf("unexpected negative duration=%q %v", name, value)
	}
}

func TestNormalizeCheckArgs(t *testing.T) {
	got := normalizeCheckArgs([]string{"check-a", "--root", "/tmp/root", "check-b", "--verbose"})
	want := []string{"--root", "/tmp/root", "--verbose", "check-a", "check-b"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("normalized=%v want=%v", got, want)
	}
}
