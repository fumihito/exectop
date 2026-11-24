// Package archive creates and expands portable exectop.d bundles.
package archive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const manifestName = ".archive.data"

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

type Manifest struct {
	Format    string `json:"format"`
	ArchiveID string `json:"archive_id"`
	CreatedAt string `json:"created_at"`
	Files     []File `json:"files"`
}

type Mode string

const (
	Merge   Mode = "merge"
	Replace Mode = "replace"
	Refuse  Mode = "refuse"
)

func Create(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("archive source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("archive source is not a directory: %s", source)
	}
	files, err := collect(source)
	if err != nil {
		return err
	}
	manifest := Manifest{Format: "exectop.d/v1", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Files: files}
	manifest.ArchiveID = manifestID(manifest.Files)

	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer out.Close()
	gw := gzip.NewWriter(out)
	tw := tar.NewWriter(gw)
	for _, f := range files {
		if err := addFile(tw, filepath.Join(source, filepath.FromSlash(f.Path)), f); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	h := &tar.Header{Name: manifestName, Mode: 0600, Size: int64(len(data)), ModTime: time.Now()}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return out.Close()
}

func Extract(source, destination string, mode Mode) error {
	if mode != Merge && mode != Replace && mode != Refuse {
		return fmt.Errorf("invalid archive mode %q (want merge, replace, or refuse)", mode)
	}
	data, err := readSource(source)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "exectop-archive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	manifest, err := unpack(data, stage)
	if err != nil {
		return err
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if err := verifyFiles(stage, manifest); err != nil {
		return err
	}
	if info, err := os.Stat(destination); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("archive destination is not a directory: %s", destination)
		}
		if mode == Refuse {
			return fmt.Errorf("archive destination already exists: %s", destination)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mode == Replace {
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return err
		}
		if err := os.RemoveAll(destination); err != nil {
			return fmt.Errorf("replace archive destination: %w", err)
		}
		if err := os.Rename(stage, destination); err != nil {
			return err
		}
		return writeManifest(destination, manifest)
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	return merge(stage, destination, manifest)
}

func collect(root string) ([]File, error) {
	var files []File
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if filepath.Base(path) == manifestName && rel == manifestName {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive source contains unsupported symlink: %s", rel)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive source contains unsupported file: %s", rel)
		}
		hash, err := fileHash(path)
		if err != nil {
			return err
		}
		files = append(files, File{Path: filepath.ToSlash(rel), SHA256: hash, Mode: uint32(info.Mode().Perm())})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func addFile(tw *tar.Writer, path string, f File) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	h := &tar.Header{Name: f.Path, Mode: int64(f.Mode), Size: info.Size(), ModTime: info.ModTime()}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(tw, in)
	return err
}

func unpack(data []byte, stage string) (Manifest, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return Manifest{}, fmt.Errorf("read archive gzip: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	var manifest Manifest
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read archive tar: %w", err)
		}
		name, err := safeName(h.Name)
		if err != nil || seen[name] {
			if err != nil {
				return Manifest{}, err
			}
			return Manifest{}, fmt.Errorf("archive contains duplicate path: %s", name)
		}
		seen[name] = true
		if name == manifestName {
			if h.Typeflag != tar.TypeReg {
				return Manifest{}, fmt.Errorf("archive metadata is not a regular file")
			}
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return Manifest{}, err
			}
			if err := json.Unmarshal(b, &manifest); err != nil {
				return Manifest{}, fmt.Errorf("parse archive metadata: %w", err)
			}
			continue
		}
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(filepath.Join(stage, filepath.FromSlash(name)), 0700); err != nil {
				return Manifest{}, err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return Manifest{}, fmt.Errorf("archive contains unsupported entry: %s", name)
		}
		target := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return Manifest{}, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0777)
		if err != nil {
			return Manifest{}, err
		}
		_, copyErr := io.CopyN(out, tr, h.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return Manifest{}, copyErr
		}
		if closeErr != nil {
			return Manifest{}, closeErr
		}
	}
	return manifest, nil
}

func merge(stage, destination string, manifest Manifest) error {
	old := map[string]File{}
	metadata := filepath.Join(destination, manifestName)
	if data, err := os.ReadFile(metadata); err == nil {
		var previous Manifest
		if json.Unmarshal(data, &previous) == nil {
			for _, f := range previous.Files {
				old[f.Path] = f
			}
		}
	}
	incoming := map[string]File{}
	for _, f := range manifest.Files {
		incoming[f.Path] = f
		if preserveLocal(destination, old[f.Path], f.Path) {
			continue
		}
		if err := copyFile(filepath.Join(stage, filepath.FromSlash(f.Path)), filepath.Join(destination, filepath.FromSlash(f.Path)), os.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	for path, previous := range old {
		if _, present := incoming[path]; present || preserveLocal(destination, previous, path) {
			continue
		}
		if err := os.Remove(filepath.Join(destination, filepath.FromSlash(path))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return writeManifest(destination, manifest)
}

func writeManifest(destination string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, manifestName), data, 0600)
}

func preserveLocal(root string, previous File, path string) bool {
	if previous.Path == "" {
		return false
	}
	hash, err := fileHash(filepath.Join(root, filepath.FromSlash(path)))
	return err == nil && hash != previous.SHA256
}

func copyFile(source, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Chmod(mode.Perm())
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func validateManifest(m Manifest) error {
	if m.Format != "exectop.d/v1" || m.ArchiveID == "" {
		return fmt.Errorf("unsupported or incomplete archive metadata")
	}
	if m.ArchiveID != manifestID(m.Files) {
		return fmt.Errorf("archive metadata checksum mismatch")
	}
	for _, f := range m.Files {
		if _, err := safeName(f.Path); err != nil || f.SHA256 == "" {
			return fmt.Errorf("invalid archive metadata path: %s", f.Path)
		}
	}
	return nil
}

func verifyFiles(root string, manifest Manifest) error {
	for _, f := range manifest.Files {
		path := filepath.Join(root, filepath.FromSlash(f.Path))
		hash, err := fileHash(path)
		if err != nil {
			return fmt.Errorf("archive file missing: %s", f.Path)
		}
		if hash != f.SHA256 {
			return fmt.Errorf("archive file checksum mismatch: %s", f.Path)
		}
	}
	return nil
}

func manifestID(files []File) string {
	b, _ := json.Marshal(files)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeName(name string) (string, error) {
	name = filepath.ToSlash(name)
	if name == "" || name == "." || strings.HasPrefix(name, "/") || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("unsafe archive path: %q", name)
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe archive path: %q", name)
	}
	return clean, nil
}

func readSource(source string) ([]byte, error) {
	u, err := url.Parse(source)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(u.Scheme) {
	case "":
		return os.ReadFile(source)
	case "file":
		return os.ReadFile(u.Path)
	case "http", "https":
		resp, err := http.Get(source)
		if err != nil {
			return nil, fmt.Errorf("download archive: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("download archive: HTTP %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	case "ftp":
		return ftpRead(u)
	default:
		return nil, fmt.Errorf("unsupported archive source protocol: %s", u.Scheme)
	}
}

func ftpRead(u *url.URL) ([]byte, error) {
	addr := u.Host
	if !strings.Contains(addr, ":") {
		addr += ":21"
	}
	ctrl, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect FTP: %w", err)
	}
	defer ctrl.Close()
	r := ftpResponseReader{r: ctrl, br: bufio.NewReader(ctrl)}
	if err := r.expect(220); err != nil {
		return nil, err
	}
	user, pass := "anonymous", "exectop@localhost"
	if u.User != nil {
		user = u.User.Username()
		if p, ok := u.User.Password(); ok {
			pass = p
		}
	}
	if err := ftpCommand(ctrl, &r, "USER", user, 331, 230); err != nil {
		return nil, err
	}
	if user != "anonymous" || pass != "" {
		if err := ftpCommand(ctrl, &r, "PASS", pass, 230); err != nil {
			return nil, err
		}
	}
	if err := ftpCommand(ctrl, &r, "TYPE", "I", 200); err != nil {
		return nil, err
	}
	if err := ftpCommand(ctrl, &r, "PASV", "", 227); err != nil {
		return nil, err
	}
	if r.passiveHost == "" || r.passivePort == 0 {
		return nil, fmt.Errorf("invalid FTP passive response")
	}
	if r.passiveHost == "0.0.0.0" {
		r.passiveHost = u.Hostname()
	}
	port := r.passivePort
	dataConn, err := net.DialTimeout("tcp", net.JoinHostPort(r.passiveHost, strconv.Itoa(port)), 15*time.Second)
	if err != nil {
		return nil, err
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	if err := ftpCommand(ctrl, &r, "RETR", path, 150, 125); err != nil {
		dataConn.Close()
		return nil, err
	}
	data, readErr := io.ReadAll(dataConn)
	dataConn.Close()
	if readErr != nil {
		return nil, readErr
	}
	if err := r.expect(226); err != nil {
		return nil, err
	}
	return data, nil
}

type ftpResponseReader struct {
	r           net.Conn
	br          *bufio.Reader
	passiveHost string
	passivePort int
}

func (r *ftpResponseReader) expect(want ...int) error {
	line, err := r.br.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimSpace(line)
	if len(line) < 3 {
		return fmt.Errorf("invalid FTP response: %q", line)
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return err
	}
	for _, w := range want {
		if code == w {
			if code == 227 {
				parts := strings.Split(line, "(")
				if len(parts) > 1 {
					values := strings.Split(strings.TrimSuffix(parts[1], ")"), ",")
					if len(values) == 6 {
						r.passiveHost = strings.Join(values[:4], ".")
						p1, _ := strconv.Atoi(values[4])
						p2, _ := strconv.Atoi(values[5])
						r.passivePort = p1*256 + p2
					}
				}
			}
			return nil
		}
	}
	return fmt.Errorf("FTP response %d: %s", code, line)
}

func ftpCommand(conn net.Conn, r *ftpResponseReader, command, arg string, want ...int) error {
	if arg != "" {
		_, _ = fmt.Fprintf(conn, "%s %s\r\n", command, arg)
	} else {
		_, _ = fmt.Fprintf(conn, "%s\r\n", command)
	}
	return r.expect(want...)
}
