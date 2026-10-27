// Package restore restores only the five stock software partitions of A113X.
package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/gofrs/flock"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/profile"
)

type Partition struct {
	Name          string `json:"name"`
	Offset        uint32 `json:"offset"`
	Bytes         int64  `json:"bytes"`
	ExpectedBytes int64  `json:"expected_bytes"`
	SHA256        string `json:"sha256"`
}
type Plan struct {
	Input          string      `json:"input"`
	Profile        string      `json:"profile"`
	Fingerprint    string      `json:"device_fingerprint"`
	ManifestSHA256 string      `json:"manifest_sha256"`
	Partitions     []Partition `json:"partitions"`
	Bootcmd        string      `json:"bootcmd"`
	PartialData    bool        `json:"partial_data"`
	Warnings       []string    `json:"warnings,omitempty"`
}
type Session struct {
	Plan     Plan
	plan     Plan
	manifest *archive.Manifest
	lock     *flock.Flock
	closed   bool
}

func Open(ctx context.Context, dir string) (session *Session, err error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("backup input is not a directory")
	}
	lockPath := filepath.Join(root, archive.LockFileName)
	if _, e := os.Lstat(lockPath); e == nil {
		if e = archive.EnsureRegular(lockPath); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		lock.Close()
		return nil, err
	}
	if !locked {
		lock.Close()
		return nil, fmt.Errorf("another process is using this backup")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	m, err := archive.Load(root)
	if err != nil {
		return nil, err
	}
	known, ok := profile.Lookup(m.Profile.Name)
	if !ok || !reflect.DeepEqual(known, m.Profile) {
		return nil, fmt.Errorf("restore requires the supported A113X backup profile")
	}
	if m.ResumeCount != 0 {
		return nil, fmt.Errorf("resumed backup cannot be restored: snapshot consistency is unverified")
	}
	report, err := archive.Verify(ctx, root)
	if err != nil {
		return nil, err
	}
	if err := stockSnapshot(root); err != nil {
		return nil, err
	}
	manifestHash, _, err := archive.HashFile(ctx, filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}
	plan := Plan{Input: root, Profile: known.Name, Fingerprint: m.Identity, ManifestSHA256: manifestHash, Bootcmd: "run storeboot", Warnings: report.Warnings}
	for _, name := range []string{"boot_a", "boot_b", "system_a", "system_b", "data"} {
		var region profile.Region
		for _, r := range known.Partitions {
			if r.Name == name {
				region = r
			}
		}
		var file *archive.File
		for i := range m.Files {
			if m.Files[i].Name == name+".bin" {
				file = &m.Files[i]
				break
			}
		}
		if file == nil || !file.DoubleRead {
			return nil, fmt.Errorf("restore requires a twice-read %s.bin", name)
		}
		if file.Expected != int64(region.Size) || file.Size <= 0 || file.Size%int64(known.Geometry.PageSize) != 0 {
			return nil, fmt.Errorf("wrong size for %s", name)
		}
		if file.Status == "partial" {
			if name != "data" || file.Size >= file.Expected || (file.Expected-file.Size)%0x20000 != 0 {
				return nil, fmt.Errorf("partial %s cannot be restored", name)
			}
			plan.PartialData = true
		} else if file.Status != "verified" || file.Size != file.Expected {
			return nil, fmt.Errorf("%s is not fully verified", name)
		}
		plan.Partitions = append(plan.Partitions, Partition{name, region.Offset, file.Size, file.Expected, file.SHA256})
	}
	// Keep the authoritative plan separate from the public presentation copy.
	public := plan
	public.Partitions = append([]Partition(nil), plan.Partitions...)
	public.Warnings = append([]string(nil), plan.Warnings...)
	return &Session{Plan: public, plan: plan, manifest: m, lock: lock}, nil
}
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

func stockSnapshot(root string) error {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer directory.Close()
	for _, name := range []string{"boot_a.bin", "boot_b.bin"} {
		if err := archive.EnsureRegular(filepath.Join(root, name)); err != nil {
			return err
		}
		f, err := directory.Open(name)
		if err != nil {
			return err
		}
		var magic [8]byte
		_, readErr := io.ReadFull(f, magic[:])
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if !bytes.Equal(magic[:], []byte("ANDROID!")) {
			return fmt.Errorf("unsupported snapshot: %s is not a stock Android boot image", name)
		}
	}
	if err := archive.EnsureRegular(filepath.Join(root, "env.txt")); err != nil {
		return err
	}
	f, err := directory.Open("env.txt")
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 65537))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if len(data) > 65536 {
		return fmt.Errorf("unsupported snapshot: boot environment is too large")
	}
	count := 0
	for _, line := range strings.Split(strings.TrimRight(string(data), "\x00"), "\n") {
		name, value, found := strings.Cut(line, "=")
		if found && name == "bootcmd" {
			count++
			if value != "run storeboot" {
				return fmt.Errorf("unsupported snapshot: stock restore cannot restore Alpine boot settings")
			}
		}
	}
	if count != 1 {
		return fmt.Errorf("unsupported snapshot: expected one stock bootcmd")
	}
	return nil
}
