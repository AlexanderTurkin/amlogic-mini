// Package flash installs Alpine only in the fixed A113X kernel/DTB/UBI regions.
package flash

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"amlogicmini/internal/firmware"
	"amlogicmini/internal/operation"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/progress"
	"amlogicmini/internal/restore"
)

type Device interface {
	restore.ConnectionDevice
	EraseAlpine(context.Context) error
	WriteAlpineChunk(context.Context, string, uint32, []byte) error
	AlpineBoot(context.Context, uint32, uint32) error
	ReadAlpineBootImage(context.Context, string, uint32, io.Writer) (int64, error)
}
type Plan struct {
	BackupPolicy    string           `json:"backup_policy,omitempty"`
	ImagesDirectory string           `json:"images_directory"`
	Backup          string           `json:"backup"`
	Fingerprint     string           `json:"device_fingerprint"`
	Profile         string           `json:"profile"`
	Images          []firmware.Image `json:"images"`
	EraseOffset     uint32           `json:"erase_offset"`
	EraseBytes      uint32           `json:"erase_bytes"`
}
type Options struct {
	BackupPresenceConfirmed bool
	Report                  string
	Progress                progress.Notify
}
type ImageReport struct {
	firmware.Image
	Status         string `json:"status"`
	WrittenBytes   uint32 `json:"written_bytes"`
	ReadbackSHA256 string `json:"readback_sha256,omitempty"`
}
type Report struct {
	Schema                    int           `json:"schema"`
	Plan                      Plan          `json:"plan"`
	Status                    string        `json:"status"`
	Stage                     string        `json:"stage"`
	Started                   string        `json:"started_utc"`
	Finished                  string        `json:"finished_utc,omitempty"`
	Images                    []ImageReport `json:"images"`
	PreviousEnvironmentSHA256 string        `json:"previous_environment_sha256,omitempty"`
	BootEnvironmentVerified   bool          `json:"boot_environment_verified"`
	Error                     string        `json:"error,omitempty"`
}
type Session struct {
	plan   Plan
	bundle *firmware.Bundle
}

// OpenWithBackupPresence checks the user-selected existence-only backup policy.
// Firmware and the live device are independently checked before any write.
func OpenWithBackupPresence(ctx context.Context, images, backup string) (*Session, error) {
	root, err := filepath.Abs(backup)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if err = backupDirectoryExists(root); err != nil {
		return nil, err
	}
	bundle, err := firmware.Load(ctx, images)
	if err != nil {
		return nil, err
	}
	plan := Plan{BackupPolicy: "presence-only", ImagesDirectory: bundle.Directory(), Backup: root, Profile: profile.A113X().Name, Images: bundle.Images(), EraseOffset: 0x1200000, EraseBytes: 0xee00000}
	return &Session{plan: plan, bundle: bundle}, nil
}
func backupDirectoryExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("backup directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup must be a directory")
	}
	return nil
}
func (s *Session) ValidateApply(o Options) error {
	if !o.BackupPresenceConfirmed {
		return fmt.Errorf("flash requires explicit confirmation")
	}
	if o.Report == "" {
		return fmt.Errorf("flash report path is required")
	}
	return nil
}
func (s *Session) Apply(ctx context.Context, d Device, o Options) (result *Report, err error) {
	if err = s.ValidateApply(o); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = backupDirectoryExists(s.plan.Backup); err != nil {
		return nil, err
	}

	store, err := operation.NewStore(o.Report, s.plan.Backup, s.plan.ImagesDirectory)
	if err != nil {
		return nil, err
	}
	r := &Report{Schema: 2, Plan: s.plan, Status: "running", Stage: "preflight", Started: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, image := range s.plan.Images {
		r.Images = append(r.Images, ImageReport{Image: image, Status: "pending"})
	}
	if err = store.Write(r, true); err != nil {
		return nil, err
	}
	result = r
	defer func() {
		if err != nil {
			r.Status = "failed"
			if errors.Is(err, context.Canceled) {
				r.Status = "interrupted"
			}
			r.Error = err.Error()
			r.Finished = time.Now().UTC().Format(time.RFC3339Nano)
			err = errors.Join(err, store.Write(r, false))
		}
	}()
	// Capture ALL firmware snapshots before any erase; changed input can never
	// leave the device erased and waiting for an image that no longer validates.
	if o.Progress != nil {
		o.Progress(progress.Event{Stage: "source"})
	}
	snapshots, err := s.bundle.Snapshots(ctx)
	if err != nil {
		return r, err
	}
	if o.Progress != nil {
		o.Progress(progress.Event{Stage: "check"})
	}
	check, err := restore.CheckConnection(ctx, d)
	if err != nil {
		return r, err
	}
	r.PreviousEnvironmentSHA256 = check.EnvironmentSHA256
	r.Plan.Fingerprint = check.Fingerprint
	if err = backupDirectoryExists(s.plan.Backup); err != nil {
		return r, err
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	r.Stage = "erasing"
	if err = store.Write(r, false); err != nil {
		return r, err
	}
	if o.Progress != nil {
		o.Progress(progress.Event{Stage: "erase"})
	}
	if err = d.EraseAlpine(ctx); err != nil {
		return r, fmt.Errorf("erase Alpine regions: %w", err)
	}
	for i, image := range s.plan.Images {
		if o.Progress != nil {
			o.Progress(progress.Event{Stage: "write", Item: image.Name, Total: int64(image.WriteBytes)})
		}
		r.Stage = "writing:" + image.Name
		r.Images[i].Status = "writing"
		for pos := uint32(0); pos < image.WriteBytes; {
			if err = ctx.Err(); err != nil {
				return r, err
			}
			size := min(uint32(0x800000), image.WriteBytes-pos)
			payload := bytes.Repeat([]byte{255}, int(size))
			actual := min(size, image.Bytes-pos)
			copy(payload, snapshots[i][pos:pos+actual])
			if err = store.Write(r, false); err != nil {
				return r, err
			}
			if err = d.WriteAlpineChunk(ctx, image.Name, pos, payload); err != nil {
				return r, fmt.Errorf("write %s at 0x%x: %w", image.Name, pos, err)
			}
			pos += size
			r.Images[i].WrittenBytes = pos
			if err = store.Write(r, false); err != nil {
				return r, err
			}
			if o.Progress != nil {
				o.Progress(progress.Event{Stage: "write", Item: image.Name, Done: int64(pos), Total: int64(image.WriteBytes)})
			}
		}
		r.Images[i].Status = "written"
	}
	// Verify only after ALL writes, so a bad-block skip crossing an adjacent image
	// cannot be hidden by verifying the earlier image before it is overwritten.
	for i, image := range s.plan.Images {
		r.Stage = "verifying:" + image.Name
		r.Images[i].Status = "verifying"
		if err = store.Write(r, false); err != nil {
			return r, err
		}
		if o.Progress != nil {
			o.Progress(progress.Event{Stage: "verify", Item: image.Name, Total: int64(image.Bytes)})
		}
		digest := sha256.New()
		measured := progress.Writer(digest, progress.Event{Stage: "verify", Item: image.Name, Total: int64(image.Bytes)}, o.Progress)
		if image.Name == "kernel" || image.Name == "dtb" {
			n, e := d.ReadAlpineBootImage(ctx, image.Name, image.Bytes, measured)
			if e != nil {
				return r, fmt.Errorf("continuous boot read %s: %w", image.Name, e)
			}
			if n != int64(image.Bytes) {
				return r, fmt.Errorf("short continuous boot read: %s", image.Name)
			}
		} else {
			for pos := uint32(0); pos < image.WriteBytes; {
				size := min(uint32(0x800000), image.WriteBytes-pos)
				data, e := d.ReadNAND(ctx, image.Offset+pos, size, false)
				if e != nil {
					return r, fmt.Errorf("read back %s: %w", image.Name, e)
				}
				if uint32(len(data)) != size {
					return r, fmt.Errorf("short NAND readback: %s", image.Name)
				}
				actual := min(size, image.Bytes-pos)
				measured.Write(data[:actual])
				if bytes.Count(data[actual:], []byte{255}) != len(data[actual:]) {
					return r, fmt.Errorf("NAND page padding mismatch: %s", image.Name)
				}
				pos += size
			}
		}
		r.Images[i].ReadbackSHA256 = fmt.Sprintf("%x", digest.Sum(nil))
		if r.Images[i].ReadbackSHA256 != image.SHA256 {
			return r, fmt.Errorf("NAND image checksum mismatch: %s; boot configuration was not changed", image.Name)
		}
		r.Images[i].Status = "verified"
		if err = store.Write(r, false); err != nil {
			return r, err
		}
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	r.Stage = "alpine-boot-environment"
	if err = store.Write(r, false); err != nil {
		return r, err
	}
	if o.Progress != nil {
		o.Progress(progress.Event{Stage: "boot"})
	}
	if err = d.AlpineBoot(ctx, s.plan.Images[0].Bytes, s.plan.Images[1].Bytes); err != nil {
		return r, err
	}
	r.BootEnvironmentVerified = true
	r.Status = "verified"
	r.Stage = "finished"
	r.Finished = time.Now().UTC().Format(time.RFC3339Nano)
	if err = store.Write(r, false); err != nil {
		return r, err
	}
	return r, nil
}
