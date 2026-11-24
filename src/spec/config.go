package spec

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var DefaultColumns = []string{"status", "name", "summary", "interval", "history", "group", "last", "duration"}

// LoadColumns resolves the first .exectop.column found at dir or an ancestor
// up to root. A missing chain falls back to the built-in watch columns.
func LoadColumns(root, dir string) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(root, filepath.FromSlash(strings.Trim(dir, "/")))
	for {
		path := filepath.Join(dir, ".exectop.column")
		b, e := os.ReadFile(path)
		if e == nil {
			if len(b) > 4096 {
				return nil, fmt.Errorf("%s: column file exceeds 4KiB", path)
			}
			var out []string
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
				line = strings.ReplaceAll(line, ",", " ")
				out = append(out, strings.Fields(line)...)
			}
			if len(out) == 0 {
				return nil, fmt.Errorf("%s: no columns", path)
			}
			return out, nil
		}
		if !os.IsNotExist(e) {
			return nil, e
		}
		if dir == root || filepath.Dir(dir) == dir {
			break
		}
		dir = filepath.Dir(dir)
	}
	return append([]string(nil), DefaultColumns...), nil
}

// LoadDirectoryConfig reads the deliberately small TOML subset used by exectop.
// Unknown keys are retained so newer keys can be handled by Resolve without
// making old daemons unable to inspect a directory.
type DirConfig struct {
	Defaults map[string]string
	Checks   map[string]map[string]string
	Sections map[string]map[string]string
}

func LoadDirectoryConfig(path string) (DirConfig, error) {
	c := DirConfig{Defaults: map[string]string{}, Checks: map[string]map[string]string{}, Sections: map[string]map[string]string{}}
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return c, nil
	}
	if e != nil {
		return c, e
	}
	defer f.Close()
	section := ""
	var current map[string]string
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			section = strings.TrimSpace(l[1 : len(l)-1])
			current = nil
			if section == "defaults" {
				current = c.Defaults
			} else if strings.HasPrefix(section, "checks.") {
				name := strings.Trim(strings.TrimPrefix(section, "checks."), "\"")
				current = map[string]string{}
				c.Checks[name] = current
			} else {
				current = map[string]string{}
				c.Sections[section] = current
			}
			continue
		}
		if current == nil {
			return c, fmt.Errorf("%s:%d: key outside section", path, line)
		}
		kv := strings.SplitN(l, "=", 2)
		if len(kv) != 2 {
			return c, fmt.Errorf("%s:%d: invalid assignment", path, line)
		}
		k := strings.TrimSpace(kv[0])
		v := stripComment(strings.TrimSpace(kv[1]))
		if strings.HasPrefix(v, "\"") {
			if !strings.HasSuffix(v, "\"") {
				return c, fmt.Errorf("%s:%d: unterminated string", path, line)
			}
			v = strings.Trim(v, "\"")
		} else if strings.HasPrefix(v, "[") {
			v = strings.TrimSpace(v)
			if !strings.HasSuffix(v, "]") {
				return c, fmt.Errorf("%s:%d: invalid array", path, line)
			}
		}
		current[k] = v
	}
	return c, sc.Err()
}

func stripComment(v string) string {
	quoted := false
	escaped := false
	for i, r := range v {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return strings.TrimSpace(v[:i])
		}
	}
	return strings.TrimSpace(v)
}
func Chain(root, file string, id string) ([]map[string]string, error) {
	defaults, checks, e := chainParts(root, file, id)
	if e != nil {
		return nil, e
	}
	return append(withSource(defaults, "directory-default"), withSource(checks, "directory-check")...), nil
}

// ChainWithHeader returns layers in the documented precedence order below a
// CLI override: ancestor defaults, file header, then directory-specific
// settings. The caller should append the global layer separately.
func ChainWithHeader(root, file, id string, header map[string]string) ([]map[string]string, error) {
	defaults, checks, e := chainParts(root, file, id)
	if e != nil {
		return nil, e
	}
	out := make([]map[string]string, 0, len(defaults)+len(checks)+1)
	out = append(out, withSource(defaults, "directory-default")...)
	if len(header) > 0 {
		out = append(out, withSource([]map[string]string{header}, "header")...)
	}
	out = append(out, withSource(checks, "directory-check")...)
	return out, nil
}

func withSource(layers []map[string]string, source string) []map[string]string {
	out := make([]map[string]string, 0, len(layers))
	for _, layer := range layers {
		copy := make(map[string]string, len(layer)+1)
		for k, v := range layer {
			copy[k] = v
		}
		copy["__exectop_source"] = source
		out = append(out, copy)
	}
	return out
}

func chainParts(root, file string, id string) ([]map[string]string, []map[string]string, error) {
	var paths []string
	dir := filepath.Dir(file)
	for {
		paths = append(paths, filepath.Join(dir, ".exectop.toml"))
		if dir == root || dir == filepath.Dir(dir) {
			break
		}
		dir = filepath.Dir(dir)
	}
	var defaults, checks []map[string]string
	fileDir := filepath.Dir(file)
	for i := len(paths) - 1; i >= 0; i-- {
		configDir := filepath.Dir(paths[i])
		c, e := LoadDirectoryConfig(paths[i])
		if e != nil {
			return nil, nil, e
		}
		if len(c.Defaults) > 0 {
			defaults = append(defaults, c.Defaults)
		}
		if configDir == fileDir {
			base := id[strings.LastIndex(id, "/")+1:]
			if x, ok := c.Checks[base]; ok {
				checks = append(checks, x)
			}
		}
	}
	return defaults, checks, nil
}
