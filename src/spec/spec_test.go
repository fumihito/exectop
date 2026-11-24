package spec

import (
	"exectop/src/discover"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeaderAndMinimum(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: name=\"hello world\" interval=1s min_interval=2s\n"), 0700); e != nil {
		t.Fatal(e)
	}
	m, e := ParseHeader(p)
	if e != nil || m["name"] != "hello world" {
		t.Fatal(m, e)
	}
	f := discover.Found{ID: "x", Path: p}
	s, e := Resolve(f, []map[string]string{m}, nil, Overrides{})
	if e != nil || s.Interval != 2*time.Second {
		t.Fatal(s, e)
	}
}

func TestDependencyDeclarations(t *testing.T) {
	f := discover.Found{ID: "x", Path: filepath.Join(t.TempDir(), "x")}
	s, err := Resolve(f, []map[string]string{{"requires": "awk,df", "requires_os": "linux", "requires_path": "/proc/meminfo", "network": "required"}}, nil, Overrides{})
	if err != nil || strings.Join(s.Requires, ",") != "awk,df" || strings.Join(s.RequiresOS, ",") != "linux" || strings.Join(s.RequiresPath, ",") != "/proc/meminfo" || s.Network != "required" {
		t.Fatalf("dependencies=%+v err=%v", s, err)
	}
	if _, err := Resolve(f, []map[string]string{{"network": "sometimes"}}, nil, Overrides{}); err == nil {
		t.Fatal("invalid network requirement accepted")
	}
}

func TestLoadColumnsInheritsNearestDirectory(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "ops", "db")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".exectop.column"), []byte("status name owner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadColumns(root, "ops/db")
	if err != nil || strings.Join(got, ",") != "status,name,owner" {
		t.Fatalf("inherited=%v err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "ops", ".exectop.column"), []byte("name cpu\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadColumns(root, "ops/db")
	if err != nil || strings.Join(got, ",") != "name,cpu" {
		t.Fatalf("nearest=%v err=%v", got, err)
	}
	got, err = LoadColumns(root, "other")
	if err != nil || strings.Join(got, ",") != "status,name,owner" {
		t.Fatalf("root=%v err=%v", got, err)
	}
}

func TestRunbookURI(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "check")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: runbook=\"opsrunbook://service/disk?host=web-1#low-space\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	h, e := ParseHeader(p)
	if e != nil || h["runbook"] != "opsrunbook://service/disk?host=web-1#low-space" {
		t.Fatalf("header=%v err=%v", h, e)
	}
	s, e := Resolve(discover.Found{ID: "check", Path: p}, []map[string]string{{"runbook": h["runbook"], "__exectop_source": "header"}}, nil, Overrides{})
	if e != nil || s.Runbook != h["runbook"] || s.Sources["runbook"] != "header" {
		t.Fatalf("spec=%+v err=%v", s, e)
	}
	if _, e := Resolve(discover.Found{ID: "check", Path: p}, []map[string]string{{"runbook": "bad\nuri"}}, nil, Overrides{}); e == nil {
		t.Fatal("control character accepted in runbook URI")
	}
	if _, e := Resolve(discover.Found{ID: "check", Path: p}, []map[string]string{{"runbook": strings.Repeat("x", 16*1024+1)}}, nil, Overrides{}); e == nil {
		t.Fatal("oversized runbook URI accepted")
	}
}

func TestReferences(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "check")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: references=\"https://status.example.test/incidents.json,https://status.example.test/\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	h, e := ParseHeader(p)
	if e != nil {
		t.Fatal(e)
	}
	s, e := Resolve(discover.Found{ID: "check", Path: p}, []map[string]string{{"references": h["references"], "__exectop_source": "header"}}, nil, Overrides{})
	if e != nil {
		t.Fatal(e)
	}
	want := []string{"https://status.example.test/incidents.json", "https://status.example.test/"}
	if strings.Join(s.References, "|") != strings.Join(want, "|") || s.Sources["references"] != "header" {
		t.Fatalf("references=%v sources=%v", s.References, s.Sources)
	}
}

func TestShortNameDefaultsAndTrims(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "a-very-long-check-name.sh")
	f := discover.Found{ID: "group/a-very-long-check-name.sh", Path: p}

	s, err := Resolve(f, nil, nil, Overrides{})
	if err != nil || s.Name != "a-very-long-check-name.sh" {
		t.Fatalf("default short name=%q err=%v", s.Name, err)
	}

	input := "  日本語のチェック名がとても長い名前ですこれはさらに長い  "
	want := string([]rune(strings.TrimSpace(input))[:20])
	s, err = Resolve(f, []map[string]string{{"short_name": input}}, nil, Overrides{})
	if err != nil || s.Name != want {
		t.Fatalf("trimmed short name=%q err=%v", s.Name, err)
	}

	s, err = Resolve(f, []map[string]string{{"name": "  legacy name  "}}, nil, Overrides{})
	if err != nil || s.Name != "legacy name" {
		t.Fatalf("legacy name=%q err=%v", s.Name, err)
	}
}

func TestSizeSuffixes(t *testing.T) {
	f := discover.Found{ID: "x", Path: filepath.Join(t.TempDir(), "x")}
	s, err := Resolve(f, []map[string]string{{"output_limit": "64KiB"}}, nil, Overrides{})
	if err != nil || s.OutputLimit != 64*1024 {
		t.Fatalf("%d %v", s.OutputLimit, err)
	}
}

func TestChainWithHeaderPrecedence(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "group")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "check")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, ".exectop.toml"), []byte("[defaults]\ntimeout=1s\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, ".exectop.toml"), []byte("[defaults]\ntimeout=2s\n[checks.\"check\"]\ntimeout=4s\n"), 0600); e != nil {
		t.Fatal(e)
	}
	f := discover.Found{ID: "group/check", Path: p}
	layers, e := ChainWithHeader(root, p, f.ID, map[string]string{"timeout": "3s"})
	if e != nil {
		t.Fatal(e)
	}
	s, e := Resolve(f, layers, nil, Overrides{})
	if e != nil || s.Timeout != 4*time.Second {
		t.Fatalf("spec=%+v err=%v", s, e)
	}
	if s.Sources["timeout"] != "directory-check" {
		t.Fatalf("timeout source=%q", s.Sources["timeout"])
	}
	s, e = Resolve(f, layers, nil, Overrides{HasTimeout: true, Timeout: time.Second})
	if e != nil || s.Sources["timeout"] != "cli" {
		t.Fatalf("cli source=%q err=%v", s.Sources["timeout"], e)
	}
}

func TestDirectoryCheckOverrideDoesNotLeakToDescendants(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	child := filepath.Join(parent, "child")
	if e := os.MkdirAll(child, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(parent, ".exectop.toml"), []byte("[checks.\"check\"]\ntimeout=1s\n"), 0600); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(child, "check")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n"), 0700); e != nil {
		t.Fatal(e)
	}
	f := discover.Found{ID: "parent/child/check", Path: p}
	layers, e := ChainWithHeader(root, p, f.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	s, e := Resolve(f, layers, nil, Overrides{})
	if e != nil || s.Timeout != 10*time.Second {
		t.Fatalf("timeout=%v err=%v", s.Timeout, e)
	}
}

func TestResolveAllKeysAndParsingEdges(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x")
	if e := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: name=\"hello world\" description=\"desc\" interval=1s min_interval=2s min_interval_fail=3s timeout=4s grace=5s ok=[0,10] warn=[1] unknown=[3] tags=[a,b] run=once args=[--x, y] env=[A=B] cwd=sub nice=2 output_limit=2MiB inherit_env=true unknown_key=v\n"), 0700); e != nil {
		t.Fatal(e)
	}
	h, e := ParseHeader(p)
	if e != nil || h["name"] != "hello world" {
		t.Fatalf("header=%v err=%v", h, e)
	}
	f := discover.Found{ID: "x", Path: p}
	s, e := Resolve(f, []map[string]string{h}, map[string]string{"timeout": "1s"}, Overrides{HasTimeout: true, Timeout: 6 * time.Second, HasInterval: true, Interval: time.Second})
	if e != nil || s.Timeout != 6*time.Second || s.Interval != 2*time.Second || !s.RunOnce || s.OutputLimit != 2*1024*1024 || s.Cwd != filepath.Join(d, "sub") || !s.InheritEnv || len(s.Warnings) != 2 {
		t.Fatalf("spec=%+v err=%v", s, e)
	}
	if _, e = Resolve(f, []map[string]string{{"timeout": "bad"}}, nil, Overrides{}); e == nil {
		t.Fatal("invalid duration accepted")
	}
	if _, e = Resolve(f, []map[string]string{{"nice": "bad"}}, nil, Overrides{}); e == nil {
		t.Fatal("invalid integer accepted")
	}
	if _, e = Resolve(f, []map[string]string{{"output_limit": "bad"}}, nil, Overrides{}); e == nil {
		t.Fatal("invalid size accepted")
	}
	ki, ke := bytesValue("12KiB")
	mi, me := bytesValue("12MiB")
	gi, ge := bytesValue("12GiB")
	if ke != nil || me != nil || ge != nil || ki != 12*1024 || mi != 12*1024*1024 || gi != 12*1024*1024*1024 {
		t.Fatal("size suffix")
	}
	if _, e = bytesValue("bad"); e == nil {
		t.Fatal("bad size")
	}
	for key, value := range map[string]string{
		"disabled": "maybe", "run": "sometimes", "ok": "[0,bad]",
		"timeout": "-1s", "output_limit": "-1",
	} {
		if _, e = Resolve(f, []map[string]string{{key: value}}, nil, Overrides{}); e == nil {
			t.Fatalf("invalid %s accepted", key)
		}
	}
	if got := ints("[1,bad,3]"); len(got) != 2 {
		t.Fatal(got)
	}
	if got := split("[a, \"b\"]"); len(got) != 2 {
		t.Fatal(got)
	}
	if got := tokens(`a="b c" d=1`); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestParseHeaderBinaryAndConfigErrors(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "bin")
	if e := os.WriteFile(bin, []byte{0, 1, 2}, 0600); e != nil {
		t.Fatal(e)
	}
	if h, e := ParseHeader(bin); e != nil || len(h) != 0 {
		t.Fatalf("binary header=%v err=%v", h, e)
	}
	lateNUL := filepath.Join(d, "late-nul")
	body := make([]byte, 1200)
	for i := range body {
		body[i] = ' '
	}
	copy(body, []byte("#!/bin/sh\n# exectop: name=late\n"))
	body[1000] = 0
	if e := os.WriteFile(lateNUL, body, 0600); e != nil {
		t.Fatal(e)
	}
	if h, e := ParseHeader(lateNUL); e != nil || h["name"] != "late" {
		t.Fatalf("late NUL header=%v err=%v", h, e)
	}
	if _, e := ParseHeader(filepath.Join(d, "missing")); e == nil {
		t.Fatal("missing header accepted")
	}
	for name, body := range map[string]string{"outside": "x=1\n", "assign": "[defaults]\nbad\n", "quote": "[defaults]\nx=\"bad\n", "array": "[defaults]\nx=[bad\n"} {
		p := filepath.Join(d, name)
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := LoadDirectoryConfig(p); e == nil {
			t.Fatalf("config %s accepted", name)
		}
	}
	valid := filepath.Join(d, "valid.toml")
	if e := os.WriteFile(valid, []byte("[defaults]\ntimeout=1s # default\nname=\"a#b\" # hash is part of the value\n[checks.\"x\"]\ntimeout=2s\n[global]\nconcurrency=2\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadDirectoryConfig(valid)
	if e != nil || c.Defaults["timeout"] != "1s" || c.Defaults["name"] != "a#b" || c.Checks["x"]["timeout"] != "2s" || c.Sections["global"]["concurrency"] != "2" {
		t.Fatalf("config=%+v err=%v", c, e)
	}
	if c, e = LoadDirectoryConfig(filepath.Join(d, "none")); e != nil || len(c.Defaults) != 0 {
		t.Fatal(c, e)
	}
}

func TestParseHeaderBlockComment(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "check")
	if e := os.WriteFile(p, []byte("/* exectop: name=block timeout=2s */\n"), 0700); e != nil {
		t.Fatal(e)
	}
	h, e := ParseHeader(p)
	if e != nil || h["name"] != "block" || h["timeout"] != "2s" {
		t.Fatalf("header=%v err=%v", h, e)
	}
}
