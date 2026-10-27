package profile

import "fmt"

type Region struct {
	Name   string `json:"name"`
	Offset uint32 `json:"offset"`
	Size   uint32 `json:"size"`
}

type Geometry struct {
	NANDSize  uint32 `json:"nand_bytes"`
	PageSize  uint32 `json:"page_bytes"`
	OOBSize   uint32 `json:"oob_bytes"`
	ChunkSize uint32 `json:"chunk_bytes"`
}

type Profile struct {
	Name       string   `json:"name"`
	Geometry   Geometry `json:"geometry"`
	Partitions []Region `json:"partitions"`
	Reserved   []Region `json:"reserved"`
}

func Lookup(name string) (Profile, bool) {
	if name == "amlogic-a113x-nand256" {
		return A113X(), true
	}
	return Profile{}, false
}

func A113X() Profile {
	return Profile{
		Name: "amlogic-a113x-nand256", Geometry: Geometry{0x10000000, 2048, 64, 0x800000},
		Partitions: []Region{
			{"bootloader", 0, 0x200000}, {"tpl", 0x800000, 0x800000}, {"misc", 0x1000000, 0x200000},
			{"boot_a", 0x1200000, 0x1000000}, {"boot_b", 0x2200000, 0x1000000},
			{"system_a", 0x3200000, 0x4000000}, {"system_b", 0x7200000, 0x4000000}, {"data", 0xb200000, 0x4e00000},
		},
		Reserved: []Region{{"bbt", 0x280000, 0x80000}, {"env", 0x300000, 0x100000}, {"keys", 0x400000, 0x100000}, {"dtbrsv", 0x500000, 0x80000}, {"ddr", 0x580000, 0x40000}},
	}
}

func (p Profile) Validate() error {
	g := p.Geometry
	if p.Name == "" || g.NANDSize == 0 || g.PageSize == 0 || g.ChunkSize == 0 || g.ChunkSize > 0x800000 || g.ChunkSize%g.PageSize != 0 || g.NANDSize%g.ChunkSize != 0 || g.OOBSize > g.PageSize {
		return fmt.Errorf("invalid profile geometry")
	}
	names := map[string]bool{}
	for _, r := range append(append([]Region{}, p.Partitions...), p.Reserved...) {
		if r.Name == "" || r.Size == 0 || uint64(r.Offset)+uint64(r.Size) > uint64(g.NANDSize) {
			return fmt.Errorf("invalid region %q", r.Name)
		}
		key := fmt.Sprintf("%s:%x", r.Name, r.Offset)
		if names[key] {
			return fmt.Errorf("duplicate region %q", r.Name)
		}
		names[key] = true
	}
	return nil
}

func (g Geometry) RawBytes(mainBytes uint32) uint32 {
	return mainBytes / g.PageSize * (g.PageSize + g.OOBSize)
}

// AlpineRegions is independent of the stock partition table in backup manifests.
func AlpineRegions() []Region {
	return []Region{{"kernel", 0x01200000, 0x01900000}, {"dtb", 0x02b00000, 0x00100000}, {"ubi", 0x02c00000, 0x0d400000}}
}
