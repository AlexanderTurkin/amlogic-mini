package firmware

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amlogicmini/internal/testutil"
)

func TestValidBundleProducesFixedLayoutAndImmutableSnapshots(t *testing.T) {
	dir := testutil.Firmware(t)
	b, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	images := b.Images()
	if len(images) != 3 || images[0].Name != "kernel" || images[0].Offset != 0x1200000 || images[1].Offset != 0x2b00000 || images[2].Offset != 0x2c00000 {
		t.Fatal(images)
	}
	if err := os.WriteFile(filepath.Join(dir, "Image"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Snapshots(context.Background()); err == nil {
		t.Fatal("changed input accepted")
	}
}
func TestMalformedFirmwareFailsBeforePlanning(t *testing.T) {
	cases := []struct {
		name, file string
		change     func([]byte) []byte
	}{
		{"kernel magic", "Image", func(b []byte) []byte { b[56] = 0; return b }},
		{"kernel text offset", "Image", func(b []byte) []byte { binary.LittleEndian.PutUint64(b[8:], 1); return b }},
		{"FDT overflow", "a113x.dtb", func(b []byte) []byte { binary.BigEndian.PutUint32(b[8:], 0xfffffff0); return b }},
		{"FDT unsupported SoC", "a113x.dtb", func(b []byte) []byte {
			i := bytes.Index(b, []byte("amlogic,a113d"))
			b[i] = 'x'
			return b
		}},
		{"FDT wrong partition", "a113x.dtb", func(b []byte) []byte {
			for i := 56; i+8 < len(b); i += 4 {
				if binary.BigEndian.Uint32(b[i:]) == 0x1200000 && binary.BigEndian.Uint32(b[i+4:]) == 0x1900000 {
					b[i+3] = 1
					break
				}
			}
			return b
		}},
		{"UBI header CRC", "rootfs.ubi", func(b []byte) []byte { b[60] ^= 1; return b }},
		{"UBI VID CRC", "rootfs.ubi", func(b []byte) []byte { b[2048+60] ^= 1; return b }},
		{"UBI truncated", "rootfs.ubi", func(b []byte) []byte { return b[:len(b)-1] }},
		{"UBIFS superblock CRC", "rootfs.ubi", func(b []byte) []byte { b[2*131072+4096+4] ^= 1; return b }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testutil.Firmware(t)
			path := filepath.Join(dir, tc.file)
			b, _ := os.ReadFile(path)
			if err := os.WriteFile(path, tc.change(b), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(context.Background(), dir); err == nil {
				t.Fatal("malformed firmware accepted")
			}
		})
	}
}
func TestAmbiguousOrSymlinkedImageRejected(t *testing.T) {
	dir := testutil.Firmware(t)
	os.WriteFile(filepath.Join(dir, "other.ubi"), testutil.UBI(), 0600)
	if _, err := Load(context.Background(), dir); err == nil {
		t.Fatal("ambiguous rootfs accepted")
	}
	os.Remove(filepath.Join(dir, "other.ubi"))
	os.Rename(filepath.Join(dir, "Image"), filepath.Join(dir, "real-image"))
	os.Symlink("real-image", filepath.Join(dir, "Image"))
	if _, err := Load(context.Background(), dir); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestSuppliedFirmwareBundle(t *testing.T) {
	dir := os.Getenv("AMLOGIC_MINI_FIRMWARE_DIR")
	if dir == "" {
		t.Skip("explicit external firmware directory required")
	}
	bundle, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range bundle.Images() {
		t.Logf("%s %d bytes at 0x%x SHA-256 %s", image.Name, image.Bytes, image.Offset, image.SHA256)
	}
}

func TestUBILayoutCopiesMustAgreeEvenWithValidCRC(t *testing.T) {
	b := testutil.UBI()
	record := b[131072+4096 : 131072+4096+172]
	binary.BigEndian.PutUint32(record, 2)
	binary.BigEndian.PutUint32(record[168:], crc32.ChecksumIEEE(record[:168])^0xffffffff)
	if err := validateUBI(b); err == nil {
		t.Fatal("disagreeing valid-CRC volume tables accepted")
	}
}

func TestDTBLargerThanBootRAMGapIsRejected(t *testing.T) {
	dir := testutil.Firmware(t)
	path := filepath.Join(dir, "a113x.dtb")
	b, _ := os.ReadFile(path)
	// A valid large strings block changes no tree properties or physical layout.
	b = append(b, make([]byte, 0x80004-len(b))...)
	binary.BigEndian.PutUint32(b[4:], uint32(len(b)))
	binary.BigEndian.PutUint32(b[32:], uint32(len(b))-binary.BigEndian.Uint32(b[12:]))
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), dir); err == nil {
		t.Fatal("DTB overlaps kernel load RAM")
	}
}
func TestUBIReservationsAndDuplicateLogicalBlocksAreRejected(t *testing.T) {
	for _, kind := range []string{"oversized", "aggregate", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			b := testutil.UBI()
			for copyIndex := 0; copyIndex < 2; copyIndex++ {
				table := b[copyIndex*131072+4096 : copyIndex*131072+4096+128*172]
				if kind == "oversized" {
					binary.BigEndian.PutUint32(table, 1697)
				}
				if kind == "aggregate" {
					binary.BigEndian.PutUint32(table, 1000)
					other := table[172:344]
					binary.BigEndian.PutUint32(other, 1000)
					binary.BigEndian.PutUint32(other[4:], 1)
					other[12] = 1
					binary.BigEndian.PutUint16(other[14:], 4)
					copy(other[16:], "data")
					binary.BigEndian.PutUint32(other[168:], crc32.ChecksumIEEE(other[:168])^0xffffffff)
				}
				binary.BigEndian.PutUint32(table[168:], crc32.ChecksumIEEE(table[:168])^0xffffffff)
			}
			if kind == "duplicate" {
				b = append(b, b[2*131072:3*131072]...)
			}
			if err := validateUBI(b); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestUBIFreeOrDuplicateNonzeroLEBImagesAreNotSupported(t *testing.T) {
	for _, kind := range []string{"free-peb", "duplicate-leb"} {
		t.Run(kind, func(t *testing.T) {
			b := testutil.UBI()
			if kind == "free-peb" {
				extra := append([]byte(nil), b[:131072]...)
				for i := 2048; i < 2112; i++ {
					extra[i] = 255
				}
				b = append(b, extra...)
			} else {
				for copyIndex := 0; copyIndex < 2; copyIndex++ {
					r := b[copyIndex*131072+4096 : copyIndex*131072+4096+172]
					binary.BigEndian.PutUint32(r, 3)
					binary.BigEndian.PutUint32(r[168:], crc32.ChecksumIEEE(r[:168])^0xffffffff)
				}
				extra := append([]byte(nil), b[2*131072:3*131072]...)
				v := extra[2048:2112]
				binary.BigEndian.PutUint32(v[12:], 1)
				binary.BigEndian.PutUint32(v[60:], crc32.ChecksumIEEE(v[:60])^0xffffffff)
				b = append(b, extra...)
				b = append(b, extra...)
			}
			if err := validateUBI(b); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

func TestNeutralFirmwareBundleUsesHardwareProfile(t *testing.T) {
	dir := testutil.Firmware(t)
	if _, err := Load(context.Background(), dir); err != nil {
		t.Fatalf("neutral A113X bundle rejected: %v", err)
	}
}

func TestNANDHardwareConfigurationIsRequired(t *testing.T) {
	for _, kind := range []string{"controller", "registers", "chip select", "ecc mode", "ecc strength", "ecc step"} {
		t.Run(kind, func(t *testing.T) {
			b := testutil.DTB()
			switch kind {
			case "controller":
				b[bytes.Index(b, []byte("amlogic,meson-axg-nfc"))] = 'x'
			case "registers":
				pattern := binary.BigEndian.AppendUint64(nil, 0x7800)
				pattern = binary.BigEndian.AppendUint64(pattern, 0x100)
				b[bytes.Index(b, pattern)+7] ^= 1
			case "chip select":
				setFDTCell(t, b, "reg", 1)
			case "ecc mode":
				b[bytes.Index(b, []byte("hw\x00"))] = 's'
			case "ecc strength":
				setFDTCell(t, b, "nand-ecc-strength", 4)
			case "ecc step":
				setFDTCell(t, b, "nand-ecc-step-size", 1024)
			}
			if err := validateFDT(b); err == nil {
				t.Fatal("unsupported NAND configuration accepted")
			}
		})
	}
}

func setFDTCell(t *testing.T, b []byte, name string, value uint32, occurrence ...int) {
	t.Helper()
	stringsOffset := binary.BigEndian.Uint32(b[12:])
	names := b[stringsOffset:]
	nameOffset := bytes.Index(names, append([]byte(name), 0))
	if nameOffset < 0 {
		t.Fatal("fixture property missing")
	}
	pattern := binary.BigEndian.AppendUint32(nil, 3)
	pattern = binary.BigEndian.AppendUint32(pattern, 4)
	pattern = binary.BigEndian.AppendUint32(pattern, uint32(nameOffset))
	structureStart := binary.BigEndian.Uint32(b[8:])
	structureEnd := structureStart + binary.BigEndian.Uint32(b[36:])
	structure := b[structureStart:structureEnd]
	index := bytes.Index(structure, pattern)
	if len(occurrence) != 0 {
		for i := 0; i < occurrence[0] && index >= 0; i++ {
			next := bytes.Index(structure[index+len(pattern):], pattern)
			if next < 0 {
				index = -1
				break
			}
			index += len(pattern) + next
		}
	}
	if index < 0 {
		t.Fatal("fixture cell missing")
	}
	binary.BigEndian.PutUint32(b[int(structureStart)+index+12:], value)
}

func TestNANDBusAddressingIsRequired(t *testing.T) {
	for _, kind := range []string{"root address cells", "root size cells", "soc address cells", "soc size cells", "bus address cells", "bus size cells", "controller address cells", "controller size cells", "translation"} {
		t.Run(kind, func(t *testing.T) {
			b := testutil.DTB()
			switch kind {
			case "translation":
				pattern := binary.BigEndian.AppendUint64(nil, 0)
				pattern = binary.BigEndian.AppendUint64(pattern, 0xffe00000)
				pattern = binary.BigEndian.AppendUint64(pattern, 0x200000)
				b[bytes.Index(b, pattern)+15] ^= 1
			default:
				property := "#address-cells"
				if strings.Contains(kind, "size") {
					property = "#size-cells"
				}
				index := 0
				switch strings.Fields(kind)[0] {
				case "soc":
					index = 1
				case "bus":
					index = 2
				case "controller":
					index = 3
				}
				setFDTCell(t, b, property, 3, index)
			}
			if err := validateFDT(b); err == nil {
				t.Fatal("unsupported NAND addressing accepted")
			}
		})
	}
}
