package discover

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type Found struct {
	ID, Path, Group string
	Info            fs.FileInfo
	Owner           uint32
	Mode            fs.FileMode
}
type Warning struct{ Path, Reason string }
type Options struct{ FollowSymlinks, AllowUnsafe bool }

func Discover(root string, opt Options) ([]Found, []Warning, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	root, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	var warnings []Warning
	if bad, reason := unsafePath(root); bad {
		warnings = append(warnings, Warning{root, reason})
		if !opt.AllowUnsafe {
			return nil, warnings, fmt.Errorf("unsafe root: %s", reason)
		}
	}
	if !st.IsDir() {
		return nil, nil, fmt.Errorf("root is not a directory: %s", root)
	}
	var out []Found
	if os.Getuid() == 0 {
		warnings = append(warnings, Warning{root, "exectop is running as root"})
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
			if metadataName(name) {
				return nil
			}
			warnings = append(warnings, Warning{path, "excluded hidden/private name"})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root {
				if bad, reason := unsafePath(path); bad {
					warnings = append(warnings, Warning{path, reason})
					if !opt.AllowUnsafe {
						return fs.SkipDir
					}
				}
			}
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, te := filepath.EvalSymlinks(path)
			inside := te == nil && (target == root || strings.HasPrefix(target, root+string(os.PathSeparator)))
			if !opt.FollowSymlinks && !inside {
				warnings = append(warnings, Warning{path, "symlink excluded"})
				return nil
			}
			info, e = os.Stat(path)
			if e != nil {
				return nil
			}
			if !info.Mode().IsRegular() {
				return nil
			}
		}
		if !info.Mode().IsRegular() || filepath.Ext(name) == ".toml" || filepath.Ext(name) == ".md" || filepath.Ext(name) == ".txt" || filepath.Ext(name) == ".disabled" {
			if info.Mode().IsRegular() && (filepath.Ext(name) == ".toml" || filepath.Ext(name) == ".md" || filepath.Ext(name) == ".txt" || filepath.Ext(name) == ".disabled") {
				if !metadataName(name) {
					warnings = append(warnings, Warning{path, "excluded unsupported extension"})
				}
			}
			return nil
		}
		if info.Mode()&0111 == 0 {
			warnings = append(warnings, Warning{path, "excluded non-executable file"})
			return nil
		}
		unsafe := false
		if bad, reason := unsafePath(path); bad {
			warnings = append(warnings, Warning{path, reason})
			unsafe = true
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() && int(st.Uid) != 0 {
			warnings = append(warnings, Warning{path, "file is not owned by the running user or root"})
			unsafe = true
		}
		if unsafe && !opt.AllowUnsafe {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		group := ""
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			group = rel[:i]
		}
		var owner uint32
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			owner = st.Uid
		}
		out = append(out, Found{ID: rel, Path: path, Group: group, Info: info, Owner: owner, Mode: info.Mode()})
		return nil
	})
	return out, warnings, err
}

func metadataName(name string) bool {
	switch name {
	case ".exectop.column", ".keep", "README.md", "AGENTS.md", "check-template.txt":
		return true
	default:
		return false
	}
}
func unsafePath(p string) (bool, string) {
	i, e := os.Stat(p)
	if e != nil {
		return false, ""
	}
	if i.Mode().Perm()&0022 != 0 {
		return true, "path is group/other writable"
	}
	return false, ""
}
