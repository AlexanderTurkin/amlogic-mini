package firmware

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

func crcValid(data []byte) bool {
	return len(data) >= 4 && binary.BigEndian.Uint32(data[len(data)-4:]) == crc32.ChecksumIEEE(data[:len(data)-4])^0xffffffff
}
func validateUBI(data []byte) error {
	const peb = 131072
	if len(data) < 3*peb || len(data)%peb != 0 {
		return fmt.Errorf("rootfs must contain complete 128 KiB erase blocks")
	}
	var sequence uint32
	var volumeTable []byte
	declared := map[uint32]uint32{}
	seenLEBs := map[uint64]bool{}
	// First-release policy: retain 38 PEBs of headroom (34 bad-block
	// reserve and four layout/wear/atomic blocks). This is conservative.
	const usablePEBs = 1696 - 38
	rootVolume := uint32(0xffffffff)
	layoutCopies := map[uint32]bool{}
	rootSuperblock := false
	// First find and validate both volume tables before accepting rootfs content.
	for base := 0; base < len(data); base += peb {
		ec := data[base : base+64]
		if !bytes.Equal(ec[:4], []byte("UBI#")) || ec[4] != 1 || !crcValid(ec) || binary.BigEndian.Uint32(ec[16:]) != 2048 || binary.BigEndian.Uint32(ec[20:]) != 4096 {
			return fmt.Errorf("invalid EC header/geometry at erase block %d", base/peb)
		}
		seq := binary.BigEndian.Uint32(ec[24:])
		if base == 0 {
			sequence = seq
		} else if seq != sequence {
			return fmt.Errorf("mixed UBI image sequence")
		}
		vid := data[base+2048 : base+2112]
		if bytes.Count(vid, []byte{255}) == 64 {
			return fmt.Errorf("free VID erase blocks are not supported in a flash bundle")
		}
		if !bytes.Equal(vid[:4], []byte("UBI!")) || vid[4] != 1 || !crcValid(vid) || (vid[5] != 1 && vid[5] != 2) {
			return fmt.Errorf("invalid VID header at erase block %d", base/peb)
		}
		id := binary.BigEndian.Uint32(vid[8:])
		lnum := binary.BigEndian.Uint32(vid[12:])
		key := uint64(id)<<32 | uint64(lnum)
		if seenLEBs[key] || vid[6] != 0 {
			return fmt.Errorf("duplicate or copied UBI logical block")
		}
		seenLEBs[key] = true
		if id == 0x7fffefff {
			if lnum > 1 || layoutCopies[lnum] {
				return fmt.Errorf("duplicate/invalid UBI layout volume")
			}
			layoutCopies[lnum] = true
			table := data[base+4096 : base+4096+128*172]
			if volumeTable == nil {
				volumeTable = bytes.Clone(table)
			} else if !bytes.Equal(volumeTable, table) {
				return fmt.Errorf("UBI volume tables disagree")
			}
			found := uint32(0xffffffff)
			var reserved uint64
			for i := 0; i < 128; i++ {
				record := table[i*172 : (i+1)*172]
				if !crcValid(record) {
					return fmt.Errorf("invalid UBI volume table CRC")
				}
				count := binary.BigEndian.Uint32(record)
				if count == 0 {
					continue
				}
				reserved += uint64(count)
				if count > usablePEBs || reserved > usablePEBs {
					return fmt.Errorf("UBI volume reservations exceed supported usable capacity (%d PEBs)", usablePEBs)
				}
				declared[uint32(i)] = count
				nameLen := binary.BigEndian.Uint16(record[14:])
				if nameLen == 0 || nameLen > 127 {
					return fmt.Errorf("invalid UBI volume name")
				}
				if string(record[16:16+nameLen]) == "rootfs" {
					if found != 0xffffffff || record[12] != 1 || binary.BigEndian.Uint32(record[4:]) != 1 || binary.BigEndian.Uint32(record[8:]) != 0 {
						return fmt.Errorf("unsupported rootfs volume layout")
					}
					found = uint32(i)
				}
			}
			if found == 0xffffffff {
				return fmt.Errorf("UBI volume table has no dynamic rootfs")
			}
			if rootVolume != 0xffffffff && rootVolume != found {
				return fmt.Errorf("UBI volume tables disagree")
			}
			rootVolume = found
		}
	}
	if len(layoutCopies) != 2 {
		return fmt.Errorf("both UBI layout volume copies are required")
	}
	for base := 0; base < len(data); base += peb {
		vid := data[base+2048 : base+2112]
		id := binary.BigEndian.Uint32(vid[8:])
		lnum := binary.BigEndian.Uint32(vid[12:])
		if id != 0x7fffefff && (declared[id] == 0 || lnum >= declared[id]) {
			return fmt.Errorf("UBI logical block exceeds declared volume reservation")
		}
		if binary.BigEndian.Uint32(vid[8:]) != rootVolume || binary.BigEndian.Uint32(vid[12:]) != 0 {
			continue
		}
		if rootSuperblock {
			return fmt.Errorf("duplicate rootfs logical block zero")
		}
		rootSuperblock = true
		sb := data[base+4096 : base+8192]
		size := binary.LittleEndian.Uint32(sb[16:])
		if binary.LittleEndian.Uint32(sb) != 0x06101831 || size != 4096 || sb[20] != 6 || binary.LittleEndian.Uint32(sb[4:]) != crc32.ChecksumIEEE(sb[8:])^0xffffffff || binary.LittleEndian.Uint32(sb[32:]) != 2048 || binary.LittleEndian.Uint32(sb[36:]) != 126976 {
			return fmt.Errorf("invalid UBIFS superblock/geometry")
		}
	}
	if !rootSuperblock {
		return fmt.Errorf("rootfs UBIFS superblock is missing")
	}
	return nil
}
