// Package examples contains the example checks bundled into the exectop
// binary at build time.
package examples

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// files is populated by build with the current contents of
// exectop.d/examples. Keeping the source tree outside this package is
// intentional: the binary must work after the source tree is removed.
//
//go:embed files/.keep files/**
var files embed.FS

// Available reports whether this build contains example files.
func Available() bool {
	found := false
	_ = fs.WalkDir(files, "files", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && path != "files" && !entry.IsDir() && filepath.Base(path) != ".keep" {
			found = true
		}
		return nil
	})
	return found
}

// Install copies the bundled examples below target. target is created when it
// does not exist; generation is disabled only when this build contains no
// bundled examples.
func Install(target string) error {
	if !Available() {
		return fmt.Errorf("example generation disabled: no examples were bundled")
	}
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			info, err = os.Stat(target)
		} else {
			return err
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("cannot generate examples: root is not a directory: %s", target)
	}

	return fs.WalkDir(files, "files", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "files" {
			return nil
		}
		rel := strings.TrimPrefix(path, "files/")
		destination := filepath.Join(target, "examples", filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0755)
		}
		data, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if filepath.Ext(destination) != ".md" && filepath.Ext(destination) != ".txt" && filepath.Base(destination) != ".exectop.column" {
			mode = 0755
		}
		return os.WriteFile(destination, data, mode)
	})
}
