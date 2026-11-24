package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"exectop/src/coordinator"
	"exectop/src/discover"
	"exectop/src/persist"
	"exectop/src/render"
	"exectop/src/rpc"
	"exectop/src/runner"
	"exectop/src/spec"
	"exectop/src/status"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func BenchmarkDiscover1000(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 1000; i++ {
		p := filepath.Join(root, "g", "check-"+itoa(i))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0700); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := discover.Discover(root, discover.Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHeaderParse(b *testing.B) {
	p := filepath.Join(b.TempDir(), "check")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n# exectop: name=probe interval=3s timeout=2s tags=ops\n"), 0700); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := spec.ParseHeader(p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkResolveChain(b *testing.B) {
	f := discover.Found{ID: "g/check", Path: filepath.Join(b.TempDir(), "g", "check")}
	layers := []map[string]string{
		{"timeout": "1s", "interval": "2s"},
		{"timeout": "2s"},
		{"timeout": "3s"},
		{"timeout": "4s"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := spec.Resolve(f, layers, map[string]string{"grace": "1s"}, spec.Overrides{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRunnerTrue(b *testing.B) {
	s := spec.Spec{ID: "true", Path: "/bin/true", Cwd: b.TempDir(), Timeout: time.Second, Grace: time.Second, OutputLimit: 64 * 1024, OK: []int{0}, AllowUnsafe: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := runner.Run(context.Background(), s, runner.Meta{Iter: i + 1, Trigger: "manual", Mode: "run"}); r.Status != status.OK {
			b.Fatalf("result=%+v", r)
		}
	}
}

type benchmarkLimited struct {
	b   bytes.Buffer
	max int
}

func (w *benchmarkLimited) Write(p []byte) (int, error) {
	if w.b.Len() < w.max {
		n := w.max - w.b.Len()
		if n > len(p) {
			n = len(p)
		}
		_, _ = w.b.Write(p[:n])
	}
	return len(p), nil
}

func BenchmarkBoundedBufferWrite(b *testing.B) {
	p := bytes.Repeat([]byte("x"), 1024*1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &benchmarkLimited{max: 64 * 1024}
		_, _ = io.Copy(w, bytes.NewReader(p))
	}
}

func BenchmarkFreshnessDecide(b *testing.B) {
	c := coordinator.New(nil, func(_ context.Context, s spec.Spec, _ runner.Meta) runner.Result {
		now := time.Now()
		return runner.Result{ID: s.ID, Status: status.OK, Start: now, End: now}
	})
	c.Add(spec.Spec{ID: "x", MinInterval: time.Hour, OK: []int{0}})
	if _, err := c.Request(context.Background(), "x", coordinator.Freshness{Force: true}, "request"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Request(context.Background(), "x", coordinator.Freshness{}, "request"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshot500(b *testing.B) {
	c := coordinator.New(nil, nil)
	for i := 0; i < 500; i++ {
		c.Add(spec.Spec{ID: "check-" + itoa(i)})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Snapshot()
	}
}

func BenchmarkWriterBatch200(b *testing.B) {
	dir := b.TempDir()
	db, err := persist.Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	s := spec.Spec{ID: "x", Hash: "h"}
	if err := persist.UpsertCheck(context.Background(), db, s, ""); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := persist.NewWriterWithConfig(db, persist.WriterConfig{QueueSize: 256, BatchSize: 200, BatchInterval: time.Hour})
		for j := 0; j < 200; j++ {
			now := time.Now()
			if !w.Enqueue(persist.Item{Result: runner.Result{ID: "x", Iter: i*200 + j + 1, Status: status.OK, Start: now, End: now, SpecHash: "h"}}) {
				b.Fatal("enqueue failed")
			}
		}
		if err := w.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNDJSONRoundtrip(b *testing.B) {
	v := rpc.Request{ID: 1, Method: "run", Params: json.RawMessage(`{"wait":false}`)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := rpc.Write(&buf, v); err != nil {
			b.Fatal(err)
		}
		if _, err := rpc.Read(&buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRender500(b *testing.B) {
	rs := make([]runner.Result, 500)
	for i := range rs {
		rs[i] = runner.Result{ID: "check-" + itoa(i), Status: status.OK, ExitCode: 0, Summary: strings.Repeat("x", 20), Duration: time.Millisecond}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = render.Table(rs)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var out [20]byte
	p := len(out)
	for i > 0 {
		p--
		out[p] = byte('0' + i%10)
		i /= 10
	}
	return string(out[p:])
}
