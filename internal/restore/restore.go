package restore

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

	"amlogicmini/internal/archive"
	"amlogicmini/internal/operation"
	"amlogicmini/internal/progress"
)

// ConnectionDevice exposes only loader identity and scratch RAM operations.
type ConnectionDevice interface {
	Identify(context.Context) ([7]byte, error)
	Prime(context.Context) error
	Environment(context.Context) ([]byte, error)
	ReadNAND(context.Context, uint32, uint32, bool) ([]byte, error)
	ReadRAM(context.Context, uint32, uint32) ([]byte, error)
	WriteRAM(context.Context, uint32, []byte) error
}

type Device interface {
	ConnectionDevice
	ReadPartition(context.Context, string, uint32, io.Writer) (int64, error)
	WritePartition(context.Context, string, uint32, io.Reader, func(uint32)) error
	StockBoot(context.Context) error
}
type Options struct {
	ConfirmDevice    string
	AllowPartialData bool
	Report           string
	Progress         progress.Notify
}
type DeviceCheck struct {
	Fingerprint       string `json:"device_fingerprint"`
	RAMVerified       bool   `json:"ram_verified"`
	EnvironmentSHA256 string `json:"environment_sha256"`
}

func (s *Session) ValidateApply(o Options) error {
	if s.closed {
		return fmt.Errorf("restore session is closed")
	}
	if o.ConfirmDevice != s.plan.Fingerprint {
		return fmt.Errorf("--confirm-device must match the full fingerprint in the restore plan")
	}
	if s.plan.PartialData && !o.AllowPartialData {
		return fmt.Errorf("data.bin is partial; inspect the plan and pass --allow-partial-data explicitly")
	}
	if !s.plan.PartialData && o.AllowPartialData {
		return fmt.Errorf("--allow-partial-data is unnecessary for a complete data.bin")
	}
	if o.Report == "" {
		return fmt.Errorf("restore report path is required")
	}
	return nil
}

// CheckDevice performs only reads and a scratch RAM round trip; it never writes NAND.
func (s *Session) CheckDevice(ctx context.Context, d ConnectionDevice) (DeviceCheck, error) {
	var check DeviceCheck
	if s.closed {
		return check, fmt.Errorf("restore session is closed")
	}
	return checkConnection(ctx, d, s.manifest.Firmware, s.plan.Fingerprint)
}

// CheckConnection checks the known A113X loader, stable live keys and scratch
// RAM without relying on a backup archive or writing NAND.
func CheckConnection(ctx context.Context, d ConnectionDevice) (DeviceCheck, error) {
	return checkConnection(ctx, d, [7]byte{0, 7, 0, 16}, "")
}
func checkConnection(ctx context.Context, d ConnectionDevice, expectedFirmware [7]byte, expectedFingerprint string) (DeviceCheck, error) {
	var check DeviceCheck
	if err := ctx.Err(); err != nil {
		return check, err
	}
	firmware, err := d.Identify(ctx)
	if err != nil {
		return check, err
	}
	if firmware != expectedFirmware {
		return check, fmt.Errorf("bootloader response does not match the source backup")
	}
	if err = d.Prime(ctx); err != nil {
		return check, err
	}
	var first []byte
	for pass := 0; pass < 2; pass++ {
		keys, err := d.ReadNAND(ctx, 0x400000, 0x100000, false)
		if err != nil {
			return check, fmt.Errorf("read device fingerprint: %w", err)
		}
		if len(keys) != 0x100000 {
			return check, fmt.Errorf("short device key read")
		}
		if pass == 0 {
			first = keys
		} else if !bytes.Equal(first, keys) {
			return check, fmt.Errorf("two device key reads disagree")
		}
	}
	check.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(first))
	if expectedFingerprint != "" && check.Fingerprint != expectedFingerprint {
		return check, fmt.Errorf("this device does not match the backup fingerprint")
	}
	env, err := d.Environment(ctx)
	if err != nil {
		return check, fmt.Errorf("capture current environment: %w", err)
	}
	check.EnvironmentSHA256 = fmt.Sprintf("%x", sha256.Sum256(env))
	// Cross a media-packet boundary while remaining entirely in scratch RAM.
	pattern := make([]byte, 65536+2048)
	for i := range pattern {
		pattern[i] = byte(i*37 + 17)
	}
	if err = d.WriteRAM(ctx, 0x1080000, pattern); err != nil {
		return check, fmt.Errorf("test RAM upload: %w", err)
	}
	readback, err := d.ReadRAM(ctx, 0x1080000, uint32(len(pattern)))
	if err != nil {
		return check, fmt.Errorf("test RAM readback: %w", err)
	}
	if !bytes.Equal(pattern, readback) {
		return check, fmt.Errorf("RAM write/readback mismatch; NAND was not written")
	}
	check.RAMVerified = true
	return check, nil
}

func (s *Session) Apply(ctx context.Context, d Device, o Options) (result *Report, err error) {
	if err = s.ValidateApply(o); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	h, _, err := archive.HashFile(ctx, filepath.Join(s.plan.Input, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if h != s.plan.ManifestSHA256 {
		return nil, fmt.Errorf("source manifest changed after planning")
	}
	store, err := operation.NewStore(o.Report, s.plan.Input)
	if err != nil {
		return nil, err
	}
	r := &Report{Schema: 1, Plan: s.plan, Status: "running", Stage: "checking-device", Started: time.Now().UTC().Format(time.RFC3339Nano), PartialDataAllowed: o.AllowPartialData}
	for _, p := range s.plan.Partitions {
		r.Partitions = append(r.Partitions, PartitionReport{Partition: p, Status: "pending"})
	}
	if err = store.Write(r, true); err != nil {
		return nil, fmt.Errorf("create restore report: %w", err)
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
	if o.Progress != nil {
		o.Progress(progress.Event{Stage: "check"})
	}
	check, err := s.CheckDevice(ctx, d)
	if err != nil {
		return r, err
	}
	r.EnvironmentSHA256 = check.EnvironmentSHA256
	for i, p := range s.plan.Partitions {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		// Pin a verified bounded snapshot in memory before any command for this file.
		if o.Progress != nil {
			o.Progress(progress.Event{Stage: "source", Item: p.Name})
		}
		data, e := s.source(ctx, p)
		if e != nil {
			return r, e
		}
		r.Stage = "writing:" + p.Name
		r.Partitions[i].Status = "writing"
		if err = store.Write(r, false); err != nil {
			return r, err
		}
		if o.Progress != nil {
			o.Progress(progress.Event{Stage: "write", Item: p.Name, Total: p.Bytes})
		}
		sent := sha256.New()
		reader := io.TeeReader(bytes.NewReader(data), sent)
		if err = d.WritePartition(ctx, p.Name, uint32(p.Bytes), reader, func(done uint32) {
			if o.Progress != nil {
				o.Progress(progress.Event{Stage: "write", Item: p.Name, Done: int64(done), Total: p.Bytes})
			}
		}); err != nil {
			return r, fmt.Errorf("write %s: %w", p.Name, err)
		}
		if fmt.Sprintf("%x", sent.Sum(nil)) != p.SHA256 {
			return r, fmt.Errorf("sent stream did not match %s", p.Name)
		}
		r.Stage = "verifying:" + p.Name
		r.Partitions[i].Status = "verifying"
		if err = store.Write(r, false); err != nil {
			return r, err
		}
		if o.Progress != nil {
			o.Progress(progress.Event{Stage: "verify", Item: p.Name, Total: p.Bytes})
		}
		readback := sha256.New()
		n, e := d.ReadPartition(ctx, p.Name, uint32(p.Bytes), progress.Writer(readback, progress.Event{Stage: "verify", Item: p.Name, Total: p.Bytes}, o.Progress))
		if e != nil {
			return r, fmt.Errorf("read back %s: %w", p.Name, e)
		}
		hash := fmt.Sprintf("%x", readback.Sum(nil))
		r.Partitions[i].ReadbackSHA256 = hash
		if n != p.Bytes || hash != p.SHA256 {
			return r, fmt.Errorf("readback checksum/size mismatch: %s", p.Name)
		}
		r.Partitions[i].Status = "verified"
		if err = store.Write(r, false); err != nil {
			return r, err
		}
	}
	if err = ctx.Err(); err != nil {
		return r, err
	}
	r.Stage = "stock-bootcmd"
	if err = store.Write(r, false); err != nil {
		return r, err
	}
	if o.Progress != nil {
		o.Progress(progress.Event{Stage: "boot"})
	}
	if err = d.StockBoot(ctx); err != nil {
		return r, fmt.Errorf("restore stock boot environment: %w", err)
	}
	r.StockBootVerified = true
	r.Stage = "finished"
	r.Status = "verified"
	if s.plan.PartialData {
		r.Status = "verified-with-partial-data"
	}
	r.Finished = time.Now().UTC().Format(time.RFC3339Nano)
	if err = store.Write(r, false); err != nil {
		return r, err
	}
	return r, nil
}

func (s *Session) source(ctx context.Context, p Partition) (data []byte, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.plan.Input, p.Name+".bin")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular source file: %s", p.Name)
	}
	root, err := os.OpenRoot(s.plan.Input)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(p.Name + ".bin")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() != p.Bytes {
		return nil, fmt.Errorf("source changed: %s", p.Name)
	}
	data, err = archive.ReadExact(ctx, f, p.Bytes)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != p.Bytes || fmt.Sprintf("%x", sha256.Sum256(data)) != p.SHA256 {
		return nil, fmt.Errorf("source checksum/size changed: %s", p.Name)
	}
	if (p.Name == "boot_a" || p.Name == "boot_b") && !bytes.HasPrefix(data, []byte("ANDROID!")) {
		return nil, fmt.Errorf("unsupported stock boot snapshot: %s", p.Name)
	}
	return data, nil
}
