package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateExtractAndMerge(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "exectop.d")
	writeExecutable(t, filepath.Join(source, "check.sh"), "old")
	archiveOne := filepath.Join(t.TempDir(), "one.tar.gz")
	if err := Create(source, archiveOne); err != nil {
		t.Fatal(err)
	}
	if err := Extract(archiveOne, destination, Replace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, manifestName)); err != nil {
		t.Fatalf("metadata missing: %v", err)
	}

	writeExecutable(t, filepath.Join(source, "check.sh"), "new")
	writeExecutable(t, filepath.Join(source, "new.sh"), "new file")
	archiveTwo := filepath.Join(t.TempDir(), "two.tar.gz")
	if err := Create(source, archiveTwo); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(destination, "check.sh"), "local edit")
	if err := Extract(archiveTwo, destination, Merge); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(destination, "check.sh")); got != "local edit" {
		t.Fatalf("merge overwrote local edit: %q", got)
	}
	if got := readFile(t, filepath.Join(destination, "new.sh")); got != "new file" {
		t.Fatalf("merge did not add new file: %q", got)
	}

	if err := Extract(archiveTwo, destination, Refuse); err == nil {
		t.Fatal("refuse accepted an existing destination")
	}
}

func TestMergeUpdatesUnchangedLocalFiles(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "exectop.d")
	writeExecutable(t, filepath.Join(source, "check.sh"), "old")
	first := filepath.Join(t.TempDir(), "first.tar.gz")
	if err := Create(source, first); err != nil {
		t.Fatal(err)
	}
	if err := Extract(first, destination, Replace); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(source, "check.sh"), "new")
	second := filepath.Join(t.TempDir(), "second.tar.gz")
	if err := Create(source, second); err != nil {
		t.Fatal(err)
	}
	if err := Extract(second, destination, Merge); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(destination, "check.sh")); got != "new" {
		t.Fatalf("unchanged local file was not updated: %q", got)
	}
}

func TestExtractSupportsHTTP(t *testing.T) {
	source := t.TempDir()
	writeExecutable(t, filepath.Join(source, "check.sh"), "remote")
	archivePath := filepath.Join(t.TempDir(), "remote.tar.gz")
	if err := Create(source, archivePath); err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(archivePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local HTTP listener unavailable: %v", err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "exectop.d")
	if err := Extract("http://"+listener.Addr().String()+"/bundle.tar.gz", destination, Replace); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(destination, "check.sh")); got != "remote" {
		t.Fatalf("remote archive content=%q", got)
	}
}

func TestExtractRejectsUnsafeTarPath(t *testing.T) {
	data := tarData(t, "../escape", []byte("bad"))
	destination := filepath.Join(t.TempDir(), "exectop.d")
	if err := Extract(writeBytes(t, data), destination, Replace); err == nil || !strings.Contains(err.Error(), "unsafe archive path") {
		t.Fatalf("unsafe path error=%v", err)
	}
}

func tarData(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gw := gzip.NewWriter(&b)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeBytes(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
