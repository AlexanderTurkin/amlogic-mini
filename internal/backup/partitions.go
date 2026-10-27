package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/progress"
)

func (s *session) partitions(ctx context.Context) error {

	s.progress(progress.Event{Stage: "environment"})
	data, err := s.device.Environment(ctx)
	if err != nil {
		return err
	}
	again, err := s.device.ReadRAM(ctx, 0x02000000, 65536)
	if err != nil {
		return err
	}
	if len(data) != 65536 || !bytes.Equal(data, again) {
		return fmt.Errorf("environment snapshot RAM transfer mismatch")
	}
	if err := s.recordBytes(ctx, "env.raw.bin", data, true); err != nil {
		return err
	}
	text := bytes.SplitN(data, []byte{0}, 2)[0]
	if len(text) == 0 || !bytes.Contains(text, []byte("=")) {
		return fmt.Errorf("invalid exported environment")
	}
	if err := s.recordBytes(ctx, "env.txt", text, true); err != nil {
		return err
	}
	for _, r := range s.options.Profile.Partitions {
		s.progress(progress.Event{Stage: "read", Item: r.Name, Total: int64(r.Size)})
		if err := s.partition(ctx, r); err != nil {
			return err
		}
	}
	for _, r := range s.options.Profile.Reserved {
		name := "rsv-" + r.Name + ".bin"
		// Keys were captured twice while establishing the device identity.
		if r.Name == "keys" {
			continue
		}
		s.progress(progress.Event{Stage: "read", Item: "reserved " + r.Name})
		data, err := s.readRegion(ctx, r)
		if err != nil {
			return err
		}
		if err := s.recordBytes(ctx, name, data, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) alive(ctx context.Context) error {
	version, err := s.device.Identify(ctx)
	if err != nil {
		return err
	}
	if version != s.manifest.Firmware {
		return fmt.Errorf("bootloader identity changed during read")
	}
	return s.device.Prime(ctx)
}

func (s *session) partition(ctx context.Context, r profile.Region) error {
	name := r.Name + ".bin"
	partial := name + ".partial"
	path := filepath.Join(s.options.Output, partial)
	if _, err := os.Lstat(path); err == nil {
		if err := archive.EnsureRegular(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := os.OpenRoot(s.options.Output)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	n, readErr := s.device.ReadPartition(ctx, r.Name, r.Size, progress.Writer(f, progress.Event{Stage: "read", Item: r.Name, Total: int64(r.Size)}, s.options.Progress))
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("no data read from %s: %w", r.Name, readErr)
	}
	if n > int64(r.Size) {
		return fmt.Errorf("partition %s exceeded expected size", r.Name)
	}
	if readErr != nil {
		if err := s.alive(ctx); err != nil {
			return fmt.Errorf("device disconnected while reading %s: %w", r.Name, errors.Join(readErr, err))
		}
	}
	hash := sha256.New()
	secondN, secondErr := s.device.ReadPartition(ctx, r.Name, r.Size, progress.Writer(hash, progress.Event{Stage: "verify", Item: r.Name, Total: int64(n)}, s.options.Progress))
	if err := ctx.Err(); err != nil {
		return err
	}
	if secondErr != nil {
		if err := s.alive(ctx); err != nil {
			return fmt.Errorf("device disconnected while verifying %s: %w", r.Name, errors.Join(secondErr, err))
		}
	}
	firstHash, size, err := archive.HashFile(ctx, path)
	if err != nil {
		return err
	}
	if size != n || secondN != n || firstHash != fmt.Sprintf("%x", hash.Sum(nil)) {
		return fmt.Errorf("two reads differ: %s", r.Name)
	}
	status := "verified"
	message := ""
	if n < int64(r.Size) || readErr != nil || secondErr != nil {
		status = "partial"
		message = fmt.Sprint(errors.Join(readErr, secondErr))
	}
	if err := os.Rename(path, filepath.Join(s.options.Output, name)); err != nil {
		return err
	}
	return s.record(archive.File{Name: name, Size: n, Expected: int64(r.Size), SHA256: firstHash, Status: status, DoubleRead: true, Error: message})
}
