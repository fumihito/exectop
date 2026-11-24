package discover

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverRulesAndOrder(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"z", "a", "b/c", "_hidden/x", ".dot/x"} {
		p = filepath.Join(root, p)
		if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte("x"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	f, warnings, e := Discover(root, Options{})
	if e != nil {
		t.Fatal(e)
	}
	if len(f) != 3 || f[0].ID != "a" || f[1].ID != "b/c" || f[2].ID != "z" {
		t.Fatalf("%v", f)
	}
	if os.Getuid() == 0 {
		found := false
		for _, w := range warnings {
			if w.Reason == "exectop is running as root" {
				found = true
			}
		}
		if !found {
			t.Fatalf("root warning missing: %v", warnings)
		}
	}
}

func TestAllowUnsafeReportsWarnings(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "unsafe")
	if err := os.WriteFile(p, []byte("x"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0777); err != nil {
		t.Fatal(err)
	}
	if found, _, err := Discover(root, Options{}); err != nil || len(found) != 0 {
		t.Fatalf("unsafe check was accepted: found=%v err=%v", found, err)
	}
	found, warnings, err := Discover(root, Options{AllowUnsafe: true})
	if err != nil || len(found) != 1 {
		t.Fatalf("allow unsafe: found=%v err=%v", found, err)
	}
	for _, w := range warnings {
		if w.Path == p && w.Reason == "path is group/other writable" {
			return
		}
	}
	t.Fatalf("unsafe warning missing: %v", warnings)
}

func TestBundledAuthoringMetadataIsSilentAndNotACheck(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".exectop.column", ".keep", "README.md", "AGENTS.md", "check-template.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("metadata"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	found, warnings, err := Discover(root, Options{})
	if err != nil || len(found) != 0 || len(warnings) != 0 {
		t.Fatalf("found=%v warnings=%v err=%v", found, warnings, err)
	}
}
