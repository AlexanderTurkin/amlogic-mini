// Package operation persists private operation reports bound to their storage identity.
package operation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"amlogicmini/internal/archive"
)

func reportLocation(path string, inputs ...string) (string, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", "", err
	}
	name := filepath.Base(absolute)
	if !archive.SafeName(name) {
		return "", "", fmt.Errorf("invalid report filename")
	}
	for _, input := range inputs {
		base, err := filepath.Abs(input)
		if err != nil {
			return "", "", err
		}
		if resolved, err := filepath.EvalSymlinks(base); err == nil {
			base = resolved
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		if !strings.EqualFold(filepath.VolumeName(base), filepath.VolumeName(dir)) {
			continue
		}
		rel, err := filepath.Rel(base, filepath.Join(dir, name))
		if err != nil {
			return "", "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", "", fmt.Errorf("operation report must be outside source directories")
		}
	}

	return dir, name, nil
}
func writeReport(dir, name string, r any, create bool) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if create {
		if err = archive.WriteFile(dir, name, data); err != nil {
			return err
		}
	}
	// ReplaceFile fsyncs the temporary file and, on Unix, its containing directory.
	return archive.ReplaceFile(dir, name, data)
}

// A report must stay on the same directory and remain the file we last wrote.
// In particular, losing a mounted report volume must never create a fresh report
// on the now-visible mountpoint and let destructive work continue unnoticed.
type Store struct {
	dir, name string
	directory os.FileInfo
	file      os.FileInfo
}

func NewStore(path string, inputs ...string) (*Store, error) {
	dir, name, err := reportLocation(path, inputs...)
	if err != nil {
		return nil, err
	}
	info, err := statIdentity(dir)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, name: name, directory: info}, nil
}
func (s *Store) checkDirectory() error {
	info, err := os.Stat(s.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(info, s.directory) {
		return fmt.Errorf("operation report directory changed or was unmounted")
	}
	return nil
}
func (s *Store) Write(r any, create bool) error {
	if err := s.checkDirectory(); err != nil {
		return err
	}
	target := filepath.Join(s.dir, s.name)
	if !create {
		info, err := os.Lstat(target)
		if err != nil {
			return fmt.Errorf("operation report disappeared: %w", err)
		}
		if !info.Mode().IsRegular() || s.file == nil || !os.SameFile(info, s.file) {
			return fmt.Errorf("operation report file was replaced")
		}
	}
	if err := writeReport(s.dir, s.name, r, create); err != nil {
		return err
	}
	if err := s.checkDirectory(); err != nil {
		return err
	}
	info, err := statIdentity(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("operation report is not a regular file")
	}
	s.file = info
	return nil
}

// File.Stat captures the Windows file ID immediately. Path-based Stat and Lstat
// can defer that lookup until SameFile, after the original path was replaced.
func statIdentity(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("operation report storage is not a regular file or directory")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := f.Stat()
	if err := errors.Join(statErr, f.Close()); err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("operation report storage changed while opening")
	}
	return opened, nil
}
