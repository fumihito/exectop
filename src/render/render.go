package render

import (
	"bytes"
	"encoding/json"
	"exectop/src/runner"
	"exectop/src/sanitize"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func JSONL(rs []runner.Result) string {
	var b strings.Builder
	for _, r := range rs {
		x, _ := json.Marshal(r)
		b.Write(x)
		b.WriteByte('\n')
	}
	return b.String()
}
func JSON(rs []runner.Result) []byte {
	x, _ := json.Marshal(map[string]any{"run": map[string]any{"count": len(rs)}, "results": rs})
	return x
}
func Table(rs []runner.Result) string {
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ST\tNAME\tLAST\tDUR\tCODE\tSUMMARY\tPATH")
	for _, r := range rs {
		name := shortName(r.Name, r.ID)
		last := "now"
		if r.Cached {
			last = r.Age.Round(time.Millisecond).String() + " ago (cached)"
		} else if r.Joined {
			last = "now (joined)"
		}
		code := "-"
		if r.ExitCode >= 0 {
			code = strconv.Itoa(r.ExitCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Status, sanitize.Field([]byte(name)), sanitize.Field([]byte(last)), formatDuration(r.Duration), code, sanitize.Field([]byte(r.Summary)), sanitize.Field([]byte(r.ID)))
	}
	_ = tw.Flush()
	return b.String()
}

func shortName(name, path string) string {
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(path)
	}
	runes := []rune(strings.TrimSpace(name))
	if len(runes) > 20 {
		runes = runes[:20]
	}
	return string(runes)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Millisecond {
		return "<1ms"
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	}
	s := strconv.FormatFloat(d.Seconds(), 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "s"
}
