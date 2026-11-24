package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"exectop/src/discover"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Spec struct {
	ID, Name, Description, Path, Cwd                       string
	Runbook                                                string   `json:"runbook,omitempty"`
	References                                             []string `json:"references,omitempty"`
	Args, Env                                              []string
	Interval, MinInterval, MinIntervalFail, Timeout, Grace time.Duration
	OK, Warn, Unknown                                      []int
	Tags                                                   []string
	RunOnce                                                bool
	AllowUnsafe                                            bool
	InheritEnv                                             bool
	Requires, RequiresOS, RequiresPath                     []string
	Network                                                string
	Nice                                                   int
	OutputLimit                                            int64
	Sources                                                map[string]string `json:"sources,omitempty"`
	Hash                                                   string
	InvalidReason                                          string
	Warnings                                               []string
}
type Overrides struct {
	Timeout, Interval       time.Duration
	HasTimeout, HasInterval bool
	AllowUnsafe             bool
}

func Default(f discover.Found) Spec {
	return Spec{ID: f.ID, Name: filepath.Base(f.Path), Path: f.Path, Cwd: strings.TrimSuffix(f.Path, "/"+f.ID[strings.LastIndex(f.ID, "/")+1:]), Interval: 10 * time.Second, Timeout: 10 * time.Second, Grace: 2 * time.Second, OK: []int{0}, Warn: []int{2}, Unknown: []int{3}, OutputLimit: 64 * 1024, Sources: map[string]string{"name": "builtin", "path": "builtin", "cwd": "builtin", "interval": "builtin", "timeout": "builtin", "grace": "builtin", "ok": "builtin", "warn": "builtin", "unknown": "builtin", "output_limit": "builtin", "requires": "builtin", "requires_os": "builtin", "requires_path": "builtin", "network": "builtin", "references": "builtin"}}
}
func Resolve(f discover.Found, dirs []map[string]string, global map[string]string, cli Overrides) (Spec, error) {
	s := Default(f)
	if s.Sources == nil {
		s.Sources = map[string]string{}
	}
	apply := func(m map[string]string, fallbackSource string) error {
		source := fallbackSource
		if x := m["__exectop_source"]; x != "" {
			source = x
		}
		for k, v := range m {
			if k == "__exectop_source" {
				continue
			}
			if err := set(&s, k, v); err != nil {
				return err
			}
			s.Sources[k] = source
		}
		return nil
	}
	if err := apply(global, "global"); err != nil {
		return s, err
	}
	for _, d := range dirs {
		if err := apply(d, "directory"); err != nil {
			return s, err
		}
	}
	if cli.HasTimeout {
		s.Timeout = cli.Timeout
		s.Sources["timeout"] = "cli"
	}
	if cli.HasInterval {
		s.Interval = cli.Interval
		s.Sources["interval"] = "cli"
	}
	s.AllowUnsafe = cli.AllowUnsafe
	if s.MinIntervalFail == 0 {
		s.MinIntervalFail = s.MinInterval
	}
	if s.Cwd != "" && !filepath.IsAbs(s.Cwd) {
		s.Cwd = filepath.Join(filepath.Dir(f.Path), s.Cwd)
	}
	if s.Interval < s.MinInterval {
		s.Warnings = append(s.Warnings, "interval raised to min_interval")
		s.Interval = s.MinInterval
	}
	b := fmt.Sprintf("%+v", s)
	h := sha256.Sum256([]byte(b))
	s.Hash = hex.EncodeToString(h[:])
	return s, nil
}
func ParseHeader(path string) (map[string]string, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 4096))
	if e != nil {
		return nil, e
	}
	if len(b) > 4096 {
		b = b[:4096]
	}
	if i := strings.IndexByte(string(b[:min(len(b), 512)]), 0); i >= 0 {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 32 {
		lines = lines[:32]
	}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		for _, p := range []string{"/*", "#", "//", ";", "--", "*"} {
			if strings.HasPrefix(l, p) {
				l = strings.TrimSpace(strings.TrimPrefix(l, p))
				break
			}
		}
		if !strings.HasPrefix(l, "exectop:") {
			continue
		}
		fields := tokens(strings.TrimSpace(strings.TrimPrefix(l, "exectop:")))
		for _, f := range fields {
			kv := strings.SplitN(f, "=", 2)
			if len(kv) == 2 {
				out[kv[0]] = strings.Trim(strings.TrimSpace(kv[1]), "\"")
			}
		}
	}
	return out, nil
}

func tokens(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	esc := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if esc {
			b.WriteRune(r)
			esc = false
			continue
		}
		if r == '\\' && quoted {
			esc = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			b.WriteRune(r)
			continue
		}
		if (r == ' ' || r == '\t') && !quoted {
			flush()
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return out
}
func set(s *Spec, k, v string) error {
	duration := func(value string) (time.Duration, error) {
		d, e := time.ParseDuration(value)
		if e != nil {
			return 0, e
		}
		if d < 0 {
			return 0, fmt.Errorf("duration must not be negative: %s", value)
		}
		return d, nil
	}
	intList := func(value string) ([]int, error) {
		value = strings.TrimSpace(strings.Trim(value, "[]"))
		if value == "" {
			return []int{}, nil
		}
		parts := strings.Split(value, ",")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			n, e := strconv.Atoi(strings.Trim(strings.TrimSpace(part), "\""))
			if e != nil {
				return nil, e
			}
			out = append(out, n)
		}
		return out, nil
	}
	switch k {
	case "name":
		s.Name = trimShortName(v)
	case "short_name":
		s.Name = trimShortName(v)
	case "description":
		s.Description = v
	case "runbook":
		if err := validateRunbook(v); err != nil {
			return err
		}
		s.Runbook = v
	case "references":
		s.References = split(v)
	case "cwd":
		s.Cwd = v
	case "interval":
		d, e := duration(v)
		if e != nil {
			return e
		}
		s.Interval = d
	case "min_interval":
		d, e := duration(v)
		if e != nil {
			return e
		}
		s.MinInterval = d
	case "min_interval_fail":
		d, e := duration(v)
		if e != nil {
			return e
		}
		s.MinIntervalFail = d
	case "timeout":
		d, e := duration(v)
		if e != nil {
			return e
		}
		s.Timeout = d
	case "grace":
		d, e := duration(v)
		if e != nil {
			return e
		}
		s.Grace = d
	case "tags":
		s.Tags = split(v)
	case "args":
		s.Args = split(v)
	case "env":
		s.Env = split(v)
	case "disabled":
		return fmt.Errorf("disabled is not configurable; exclude checks by path or filename")
	case "inherit_env":
		if v != "true" && v != "false" {
			return fmt.Errorf("invalid boolean: %s", v)
		}
		s.InheritEnv = v == "true"
	case "run":
		if v != "once" && v != "always" {
			return fmt.Errorf("invalid run mode: %s", v)
		}
		s.RunOnce = v == "once"
	case "nice":
		n, e := strconv.Atoi(v)
		if e != nil {
			return e
		}
		s.Nice = n
	case "output_limit":
		n, e := bytesValue(v)
		if e != nil {
			return e
		}
		s.OutputLimit = n
	case "requires":
		s.Requires = split(v)
	case "requires_os":
		s.RequiresOS = split(v)
	case "requires_path":
		s.RequiresPath = split(v)
	case "network":
		if v != "" && v != "none" && v != "optional" && v != "required" {
			return fmt.Errorf("invalid network requirement: %s", v)
		}
		s.Network = v
	case "ok":
		var e error
		s.OK, e = intList(v)
		if e != nil {
			return e
		}
	case "warn":
		var e error
		s.Warn, e = intList(v)
		if e != nil {
			return e
		}
	case "unknown":
		var e error
		s.Unknown, e = intList(v)
		if e != nil {
			return e
		}
	case "concurrency", "history":
		// Global runtime settings are parsed here as part of the shared
		// configuration chain but do not belong to an individual Spec.
	default:
		s.Warnings = append(s.Warnings, "unknown key: "+k)
	}
	return nil
}

const maxRunbookURI = 16 * 1024

func validateRunbook(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("runbook URI must not be empty")
	}
	if len(v) > maxRunbookURI {
		return fmt.Errorf("runbook URI exceeds %d bytes", maxRunbookURI)
	}
	if _, err := url.Parse(v); err != nil {
		return fmt.Errorf("invalid runbook URI: %w", err)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("runbook URI contains control character")
		}
	}
	return nil
}

func trimShortName(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > 20 {
		runes = runes[:20]
	}
	return string(runes)
}
func bytesValue(v string) (int64, error) {
	v = strings.TrimSpace(v)
	mul := int64(1)
	for suffix, factor := range map[string]int64{"GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024, "Gi": 1024 * 1024 * 1024, "Mi": 1024 * 1024, "Ki": 1024} {
		if len(v) > len(suffix) && strings.EqualFold(v[len(v)-len(suffix):], suffix) {
			mul = factor
			v = v[:len(v)-len(suffix)]
			break
		}
	}
	n, e := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if e == nil && n < 0 {
		return 0, fmt.Errorf("size must not be negative: %s", v)
	}
	return n * mul, e
}
func split(v string) []string {
	v = strings.Trim(v, "[]")
	var o []string
	for _, x := range strings.Split(v, ",") {
		x = strings.Trim(strings.TrimSpace(x), "\"")
		if x != "" {
			o = append(o, x)
		}
	}
	return o
}
func ints(v string) []int {
	var o []int
	for _, x := range split(v) {
		n, e := strconv.Atoi(x)
		if e == nil {
			o = append(o, n)
		}
	}
	return o
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
