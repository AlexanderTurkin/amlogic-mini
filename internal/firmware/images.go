// Package firmware validates the supported A113X Alpine bundle before USB use.
package firmware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/profile"
)

type Image struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Offset     uint32 `json:"offset"`
	Capacity   uint32 `json:"capacity"`
	Bytes      uint32 `json:"bytes"`
	WriteBytes uint32 `json:"write_bytes"`
	SHA256     string `json:"sha256"`
}
type Bundle struct {
	dir    string
	images []Image
}

func (b *Bundle) Directory() string { return b.dir }
func (b *Bundle) Images() []Image   { return append([]Image(nil), b.images...) }
func Load(ctx context.Context, dir string) (*Bundle, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var ubi []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ubi" {
			ubi = append(ubi, e.Name())
		}
	}
	if len(ubi) != 1 {
		return nil, fmt.Errorf("images directory must contain exactly one .ubi rootfs")
	}
	bundle := &Bundle{dir: root}
	names := []string{"Image", "a113x.dtb", ubi[0]}
	for i, r := range profile.AlpineRegions() {
		data, err := readRegular(ctx, root, names[i], r.Size)
		if err != nil {
			return nil, err
		}
		switch r.Name {
		case "kernel":
			if len(data) < 64 || !bytes.Equal(data[56:60], []byte("ARMd")) || binary.LittleEndian.Uint64(data[8:]) != 0x1080000 || binary.LittleEndian.Uint64(data[24:])&1 != 0 {
				return nil, fmt.Errorf("Image is not the adapted little-endian ARM64 kernel")
			}
			memory := binary.LittleEndian.Uint64(data[16:])
			if memory == 0 || memory > 0x10000000-0x1080000 {
				return nil, fmt.Errorf("kernel memory size is invalid")
			}
		case "dtb":
			if len(data) > 0x80000 {
				return nil, fmt.Errorf("DTB exceeds the 512 KiB boot RAM gap before the kernel")
			}
			err = validateFDT(data)
			if err != nil {
				return nil, fmt.Errorf("DTB: %w", err)
			}
		case "ubi":
			if err = validateUBI(data); err != nil {
				return nil, fmt.Errorf("rootfs UBI: %w", err)
			}
		}
		size := uint32(len(data))
		writeSize := (size + 2047) / 2048 * 2048
		if writeSize > r.Size {
			return nil, fmt.Errorf("padded %s exceeds its region", r.Name)
		}
		bundle.images = append(bundle.images, Image{r.Name, names[i], r.Offset, r.Size, size, writeSize, fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	return bundle, nil
}
func (b *Bundle) Snapshots(ctx context.Context) ([][]byte, error) {
	snapshots := make([][]byte, 0, len(b.images))
	for _, image := range b.images {
		data, err := readRegular(ctx, b.dir, image.File, image.Capacity)
		if err != nil {
			return nil, err
		}
		if uint32(len(data)) != image.Bytes || fmt.Sprintf("%x", sha256.Sum256(data)) != image.SHA256 {
			return nil, fmt.Errorf("firmware source changed: %s", image.File)
		}
		snapshots = append(snapshots, data)
	}
	return snapshots, nil
}
func readRegular(ctx context.Context, root, name string, limit uint32) (data []byte, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filepath.Join(root, name))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > int64(limit) {
		return nil, fmt.Errorf("%s is not a regular image within its region capacity", name)
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	f, err := directory.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() <= 0 || opened.Size() > int64(limit) {
		return nil, fmt.Errorf("firmware source changed: %s", name)
	}
	data, err = archive.ReadExact(ctx, f, opened.Size())
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || uint64(len(data)) > uint64(limit) {
		return nil, fmt.Errorf("%s exceeds region capacity", name)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
