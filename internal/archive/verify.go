package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/bits"
	"path/filepath"
	"slices"
)

type Report struct {
	Files    int      `json:"files"`
	Bytes    int64    `json:"bytes"`
	Warnings []string `json:"warnings,omitempty"`
}

func Verify(ctx context.Context, root string) (Report, error) {
	var report Report
	m, err := Load(root)
	if err != nil {
		return report, err
	}
	if m.Status != "complete" && m.Status != "warnings" {
		return report, fmt.Errorf("backup is incomplete (%s)", m.Status)
	}
	if m.ResumeCount > 0 {
		report.Warnings = append(report.Warnings, "backup spans capture sessions; snapshot consistency is unverified")
	}
	expected := ExpectedFiles(m.Profile, m.Mode)
	for _, f := range m.Files {
		size, ok := expected[f.Name]
		if !ok {
			return report, fmt.Errorf("unexpected backup file %s", f.Name)
		}
		if size > 0 && f.Expected != size {
			return report, fmt.Errorf("wrong expected size for %s", f.Name)
		}
		delete(expected, f.Name)
		hash, n, err := HashFile(ctx, filepath.Join(root, f.Name))
		if err != nil {
			return report, fmt.Errorf("verify %s: %w", f.Name, err)
		}
		if n != f.Size || hash != f.SHA256 {
			return report, fmt.Errorf("checksum/size mismatch: %s", f.Name)
		}
		if f.Name == "rsv-keys.bin" && hash != m.Identity {
			return report, fmt.Errorf("device fingerprint does not match key backup")
		}
		if f.Status == "partial" {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s is partial: %d/%d bytes", f.Name, n, f.Expected))
		}
		report.Files++
		report.Bytes += n
	}
	if len(expected) > 0 {
		return report, fmt.Errorf("backup is missing %d required files", len(expected))
	}
	if m.Mode != "partitions" {
		if err := verifyChunks(ctx, root, m); err != nil {
			return report, err
		}
		for _, c := range m.Chunks {
			if c.DifferentBytes > 0 {
				report.Warnings = append(report.Warnings, fmt.Sprintf("raw NAND chunk %d differs in %d bytes", c.Index, c.DifferentBytes))
			}
		}
	}
	report.Warnings = append(report.Warnings, m.Warnings...)
	return report, nil
}

func verifyChunks(ctx context.Context, root string, m *Manifest) error {
	g := m.Profile.Geometry
	if len(m.Chunks) != int(g.NANDSize/g.ChunkSize) {
		return fmt.Errorf("raw chunk map is incomplete")
	}
	var data [2][]byte
	for i, c := range m.Chunks {
		if c.Index != i || c.NANDOffset != uint32(i)*g.ChunkSize || c.Size != g.RawBytes(g.ChunkSize) || c.FileOffset != int64(i)*int64(c.Size) || !c.TransportVerified {
			return fmt.Errorf("invalid raw checkpoint %d", i)
		}
		for pass, name := range []string{"nand-full-read1.raw.bin", "nand-full-read2.raw.bin"} {
			if err := ctx.Err(); err != nil {
				return err
			}
			f, err := openRegular(filepath.Join(root, name))
			if err != nil {
				return err
			}
			if data[pass] == nil {
				data[pass] = make([]byte, c.Size)
			}
			_, readErr := f.ReadAt(data[pass], c.FileOffset)
			closeErr := f.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if fmt.Sprintf("%x", sha256.Sum256(data[pass])) != c.Hashes[pass] {
				return fmt.Errorf("committed raw chunk %d is damaged", c.Index)
			}
		}
		count, bitCount := 0, 0
		var offsets []uint32
		for offset, a := range data[0] {
			if b := data[1][offset]; a != b {
				count++
				bitCount += bits.OnesCount8(a ^ b)
				if len(offsets) < 4096 {
					offsets = append(offsets, uint32(offset))
				}
			}
		}
		if count != c.DifferentBytes || bitCount != c.DifferentBits || !slices.Equal(offsets, c.Offsets) || (count > 4096) != c.OffsetsTruncated || (bytes.Equal(data[0], data[1]) != (c.Hashes[0] == c.Hashes[1])) {
			return fmt.Errorf("raw comparison metadata contradicts captured bytes in chunk %d", i)
		}
	}
	return nil
}
