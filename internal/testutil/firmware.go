// Package testutil contains synthetic fixtures used only by tests.
package testutil

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func Firmware(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	image := make([]byte, 64)
	binary.LittleEndian.PutUint64(image[8:], 0x1080000)
	binary.LittleEndian.PutUint64(image[16:], 64)
	copy(image[56:], "ARMd")
	for name, b := range map[string][]byte{"Image": image, "a113x.dtb": DTB(), "rootfs.ubi": UBI()} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
func DTB() []byte {
	var structure, strings []byte
	names := map[string]uint32{}
	word := func(v uint32) { structure = binary.BigEndian.AppendUint32(structure, v) }
	pad := func() {
		for len(structure)%4 != 0 {
			structure = append(structure, 0)
		}
	}
	begin := func(name string) {
		word(1)
		structure = append(structure, []byte(name)...)
		structure = append(structure, 0)
		pad()
	}
	prop := func(name string, data []byte) {
		off, ok := names[name]
		if !ok {
			off = uint32(len(strings))
			names[name] = off
			strings = append(strings, []byte(name)...)
			strings = append(strings, 0)
		}
		word(3)
		word(uint32(len(data)))
		word(off)
		structure = append(structure, data...)
		pad()
	}
	number := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	begin("")
	prop("compatible", []byte("amlogic,a113d\x00"))
	prop("model", []byte("A113X test\x00"))
	prop("#address-cells", number(2))
	prop("#size-cells", number(2))
	begin("soc")
	prop("#address-cells", number(2))
	prop("#size-cells", number(2))
	prop("ranges", nil)
	begin("bus@ffe00000")
	prop("#address-cells", number(2))
	prop("#size-cells", number(2))
	var ranges []byte
	for _, v := range []uint64{0, 0xffe00000, 0x200000} {
		ranges = binary.BigEndian.AppendUint64(ranges, v)
	}
	prop("ranges", ranges)
	begin("nand-controller@7800")
	prop("#address-cells", number(1))
	prop("#size-cells", number(0))
	prop("compatible", []byte("amlogic,meson-axg-nfc\x00"))
	var registers []byte
	for _, v := range []uint64{0x7800, 0x100, 0x7000, 0x800} {
		registers = binary.BigEndian.AppendUint64(registers, v)
	}
	prop("reg", registers)
	begin("nand@0")
	prop("reg", number(0))
	prop("nand-ecc-mode", []byte("hw\x00"))
	prop("nand-ecc-strength", number(8))
	prop("nand-ecc-step-size", number(512))
	begin("partitions")
	prop("compatible", []byte("fixed-partitions\x00"))
	prop("#address-cells", number(1))
	prop("#size-cells", number(1))
	for _, r := range []struct {
		name      string
		off, size uint32
	}{{"bootloader", 0, 0x200000}, {"reserved", 0x200000, 0x600000}, {"tpl", 0x800000, 0x800000}, {"misc", 0x1000000, 0x200000}, {"kernel", 0x1200000, 0x1900000}, {"dtb", 0x2b00000, 0x100000}, {"ubi", 0x2c00000, 0xd400000}} {
		begin(r.name)
		prop("label", append([]byte(r.name), 0))
		reg := number(r.off)
		reg = append(reg, number(r.size)...)
		prop("reg", reg)
		word(2)
	}
	for i := 0; i < 6; i++ {
		word(2)
	}
	word(9)
	h := make([]byte, 56)
	vals := []uint32{0xd00dfeed, uint32(56 + len(structure) + len(strings)), 56, uint32(56 + len(structure)), 40, 17, 16, 0, uint32(len(strings)), uint32(len(structure))}
	for i, v := range vals {
		binary.BigEndian.PutUint32(h[i*4:], v)
	}
	return append(append(h, structure...), strings...)
}
func UBI() []byte {
	b := make([]byte, 3*131072)
	for i := range b {
		b[i] = 255
	}
	for block := 0; block < 3; block++ {
		base := block * 131072
		ec := b[base : base+64]
		clear(ec)
		copy(ec, "UBI#")
		ec[4] = 1
		binary.BigEndian.PutUint32(ec[16:], 2048)
		binary.BigEndian.PutUint32(ec[20:], 4096)
		binary.BigEndian.PutUint32(ec[24:], 123)
		crc(ec)
		vid := b[base+2048 : base+2112]
		clear(vid)
		copy(vid, "UBI!")
		vid[4] = 1
		vid[5] = 1
		if block < 2 {
			vid[7] = 5
			binary.BigEndian.PutUint32(vid[8:], 0x7fffefff)
			binary.BigEndian.PutUint32(vid[12:], uint32(block))
		} else {
			binary.BigEndian.PutUint32(vid[12:], 0)
		}
		crc(vid)
		if block < 2 {
			table := b[base+4096 : base+4096+128*172]
			clear(table)
			for i := 0; i < 128; i++ {
				r := table[i*172 : (i+1)*172]
				if i == 0 {
					binary.BigEndian.PutUint32(r, 1)
					binary.BigEndian.PutUint32(r[4:], 1)
					r[12] = 1
					binary.BigEndian.PutUint16(r[14:], 6)
					copy(r[16:], "rootfs")
				}
				binary.BigEndian.PutUint32(r[168:], crc32.ChecksumIEEE(r[:168])^0xffffffff)
			}
		} else {
			sb := b[base+4096 : base+8192]
			clear(sb)
			binary.LittleEndian.PutUint32(sb, 0x06101831)
			binary.LittleEndian.PutUint32(sb[16:], 4096)
			sb[20] = 6
			binary.LittleEndian.PutUint32(sb[32:], 2048)
			binary.LittleEndian.PutUint32(sb[36:], 126976)
			binary.LittleEndian.PutUint32(sb[4:], crc32.ChecksumIEEE(sb[8:])^0xffffffff)
		}
	}
	return b
}
func crc(b []byte) { binary.BigEndian.PutUint32(b[60:], crc32.ChecksumIEEE(b[:60])^0xffffffff) }
