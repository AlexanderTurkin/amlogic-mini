package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"

	"amlogicmini/internal/archive"
	"amlogicmini/internal/progress"
)

var rawNames = [2]string{"nand-full-read1.raw.bin", "nand-full-read2.raw.bin"}

func (s *session) raw(ctx context.Context) (err error) {
	g := s.options.Profile.Geometry
	chunkSize := g.RawBytes(g.ChunkSize)
	count := int(g.NANDSize / g.ChunkSize)
	var files [2]*os.File
	defer func() {
		for _, f := range files {
			if f != nil {
				err = errors.Join(err, f.Close())
			}
		}
	}()
	for pass, name := range rawNames {
		root, e := os.OpenRoot(s.options.Output)
		if e != nil {
			return e
		}
		files[pass], e = root.OpenFile(name+".partial", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		closeErr := root.Close()
		if e != nil || closeErr != nil {
			return errors.Join(e, closeErr)
		}
	}
	s.progress(progress.Event{Stage: "raw", Total: int64(count)})
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := uint32(i) * g.ChunkSize
		var captures [2][]byte
		for pass := 0; pass < 2; pass++ {
			data, e := s.device.ReadNAND(ctx, offset, g.ChunkSize, true)
			if e != nil {
				return e
			}
			if len(data) != int(chunkSize) {
				return fmt.Errorf("short physical NAND chunk %d", i)
			}
			again, e := s.device.ReadRAM(ctx, 0x01080000, chunkSize)
			if e != nil {
				return e
			}
			if !bytes.Equal(data, again) {
				return fmt.Errorf("RAM/USB transfer mismatch in raw chunk %d pass %d", i, pass+1)
			}
			captures[pass] = data
		}
		c := archive.Chunk{Index: i, NANDOffset: offset, FileOffset: int64(i) * int64(chunkSize), Size: chunkSize, Hashes: [2]string{sum(captures[0]), sum(captures[1])}, TransportVerified: true}
		for j, a := range captures[0] {
			if b := captures[1][j]; a != b {
				c.DifferentBytes++
				c.DifferentBits += bits.OnesCount8(a ^ b)
				if len(c.Offsets) < 4096 {
					c.Offsets = append(c.Offsets, uint32(j))
				} else {
					c.OffsetsTruncated = true
				}
			}
		}
		for pass, f := range files {
			n, e := f.Write(captures[pass])
			if e != nil {
				return e
			}
			if n != len(captures[pass]) {
				return fmt.Errorf("short raw file write")
			}
			if e = f.Sync(); e != nil {
				return e
			}
		}
		s.manifest.Chunks = append(s.manifest.Chunks, c)
		if err := s.save(); err != nil {
			return err
		}
		s.progress(progress.Event{Stage: "raw", Done: int64(i + 1), Total: int64(count)})
	}
	s.progress(progress.Event{Stage: "files"})
	for pass, f := range files {
		if err := f.Close(); err != nil {
			return err
		}
		files[pass] = nil
		name := rawNames[pass]
		if err := os.Rename(filepath.Join(s.options.Output, name+".partial"), filepath.Join(s.options.Output, name)); err != nil {
			return err
		}
		hash, size, err := archive.HashFile(ctx, filepath.Join(s.options.Output, name))
		if err != nil {
			return err
		}
		if err := s.record(archive.File{Name: name, Size: size, Expected: int64(g.RawBytes(g.NANDSize)), SHA256: hash, Status: "captured"}); err != nil {
			return err
		}
	}
	report, err := json.MarshalIndent(struct {
		Geometry any             `json:"geometry"`
		Chunks   []archive.Chunk `json:"chunks"`
	}{g, s.manifest.Chunks}, "", "  ")
	if err != nil {
		return err
	}
	return archive.ReplaceFile(s.options.Output, "raw-read-comparison.json", append(report, '\n'))
}
