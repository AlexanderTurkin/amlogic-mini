package firmware

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"amlogicmini/internal/profile"
)

type fdtNode struct {
	path       string
	properties map[string][]byte
}

func validateFDT(data []byte) error {
	bad := func() error { return fmt.Errorf("malformed flattened device tree") }
	if len(data) < 40 || binary.BigEndian.Uint32(data) != 0xd00dfeed {
		return bad()
	}
	h := make([]uint32, 10)
	for i := range h {
		h[i] = binary.BigEndian.Uint32(data[i*4:])
	}
	if uint64(h[1]) != uint64(len(data)) || h[5] != 17 || h[6] > 17 || h[2]%4 != 0 || h[4]%8 != 0 {
		return bad()
	}
	within := func(off, size uint32) bool { return off >= 40 && uint64(off)+uint64(size) <= uint64(len(data)) }
	if !within(h[2], h[9]) || !within(h[3], h[8]) || !within(h[4], 16) {
		return bad()
	}
	structEnd := uint64(h[2]) + uint64(h[9])
	stringsEnd := uint64(h[3]) + uint64(h[8])
	if uint64(h[2]) < stringsEnd && uint64(h[3]) < structEnd {
		return bad()
	}
	reserveEnd := int(h[4])
	for {
		if reserveEnd+16 > len(data) {
			return bad()
		}
		entry := data[reserveEnd : reserveEnd+16]
		reserveEnd += 16
		if bytes.Equal(entry, make([]byte, 16)) {
			break
		}
	}
	if uint64(h[4]) < structEnd && uint64(h[2]) < uint64(reserveEnd) || uint64(h[4]) < stringsEnd && uint64(h[3]) < uint64(reserveEnd) {
		return bad()
	}
	structure := data[h[2]:structEnd]
	names := data[h[3]:stringsEnd]
	var nodes []fdtNode
	var stack []int
	pos := 0
	ended := false
	for pos+4 <= len(structure) {
		token := binary.BigEndian.Uint32(structure[pos:])
		pos += 4
		switch token {
		case 1:
			end := bytes.IndexByte(structure[pos:], 0)
			if end < 0 || end > 255 || len(stack) >= 64 || len(nodes) >= 4096 {
				return bad()
			}
			name := string(structure[pos : pos+end])
			if strings.Contains(name, "/") {
				return bad()
			}
			path := ""
			if len(stack) == 0 {
				if len(nodes) != 0 || name != "" {
					return bad()
				}
			} else {
				path = nodes[stack[len(stack)-1]].path + "/" + name
			}
			nodes = append(nodes, fdtNode{path, map[string][]byte{}})
			stack = append(stack, len(nodes)-1)
			pos = (pos + end + 4) &^ 3
		case 2:
			if len(stack) == 0 {
				return bad()
			}
			stack = stack[:len(stack)-1]
		case 3:
			if len(stack) == 0 || pos+8 > len(structure) {
				return bad()
			}
			length := binary.BigEndian.Uint32(structure[pos:])
			off := binary.BigEndian.Uint32(structure[pos+4:])
			pos += 8
			if uint64(pos)+uint64(length) > uint64(len(structure)) || uint64(off) >= uint64(len(names)) {
				return bad()
			}
			end := bytes.IndexByte(names[off:], 0)
			if end <= 0 {
				return bad()
			}
			name := string(names[off : uint64(off)+uint64(end)])
			props := nodes[stack[len(stack)-1]].properties
			if _, exists := props[name]; exists {
				return bad()
			}
			props[name] = structure[pos : uint64(pos)+uint64(length)]
			pos = (pos + int(length) + 3) &^ 3
		case 4:
		case 9:
			if len(stack) != 0 || pos != len(structure) {
				return bad()
			}
			ended = true
		default:
			return bad()
		}
		if ended {
			break
		}
	}
	if !ended || len(nodes) == 0 {
		return bad()
	}
	compatible := nodes[0].properties["compatible"]
	if !cstringListHas(compatible, "amlogic,a113d") {
		return fmt.Errorf("device tree is not compatible with the supported A113X board")
	}
	var partitionPath string
	for _, node := range nodes {
		if cstringListHas(node.properties["compatible"], "fixed-partitions") && strings.Contains(node.path, "/nand-controller@") && strings.Contains(node.path, "/nand@") {
			if partitionPath != "" {
				return fmt.Errorf("ambiguous NAND partitions")
			}
			partitionPath = node.path
			for _, key := range []string{"#address-cells", "#size-cells"} {
				v := node.properties[key]
				if len(v) != 4 || binary.BigEndian.Uint32(v) != 1 {
					return fmt.Errorf("unsupported NAND partition cells")
				}
			}
		}
	}
	if partitionPath == "" {
		return fmt.Errorf("NAND partition layout missing")
	}
	if err := validateNANDConfiguration(nodes, partitionPath); err != nil {
		return err
	}
	expected := map[string][2]uint32{"bootloader": {0, 0x200000}, "reserved": {0x200000, 0x600000}, "tpl": {0x800000, 0x800000}, "misc": {0x1000000, 0x200000}}
	for _, r := range profile.AlpineRegions() {
		expected[r.Name] = [2]uint32{r.Offset, r.Size}
	}
	for _, node := range nodes {
		if !strings.HasPrefix(node.path, partitionPath+"/") || strings.Contains(strings.TrimPrefix(node.path, partitionPath+"/"), "/") {
			continue
		}
		label := strings.TrimSuffix(string(node.properties["label"]), "\x00")
		want, ok := expected[label]
		reg := node.properties["reg"]
		if !ok || len(reg) != 8 || binary.BigEndian.Uint32(reg) != want[0] || binary.BigEndian.Uint32(reg[4:]) != want[1] {
			return fmt.Errorf("NAND partition %q has the wrong address or size", label)
		}
		delete(expected, label)
	}
	if len(expected) != 0 {
		return fmt.Errorf("NAND partition layout is incomplete")
	}
	return nil
}

// Compatibility is based on the AXG NAND hardware and its data format,
// independently of any board's manufacturer/model strings.
func validateNANDConfiguration(nodes []fdtNode, partitions string) error {
	nandIndex := strings.LastIndex(partitions, "/")
	if nandIndex <= 0 {
		return fmt.Errorf("NAND node missing")
	}
	nandPath := partitions[:nandIndex]
	controllerIndex := strings.LastIndex(nandPath, "/")
	if controllerIndex <= 0 {
		return fmt.Errorf("NAND controller missing")
	}
	controllerPath := nandPath[:controllerIndex]
	var nand, controller *fdtNode
	for i := range nodes {
		if nodes[i].path == nandPath {
			nand = &nodes[i]
		}
		if nodes[i].path == controllerPath {
			controller = &nodes[i]
		}
	}
	if nand == nil || controller == nil || !cstringListHas(controller.properties["compatible"], "amlogic,meson-axg-nfc") {
		return fmt.Errorf("unsupported NAND controller")
	}
	if err := validateNANDBus(nodes, controller); err != nil {
		return err
	}
	registers := controller.properties["reg"]
	if len(registers) != 32 {
		return fmt.Errorf("unsupported NAND controller registers")
	}
	for i, want := range []uint64{0x7800, 0x100, 0x7000, 0x800} {
		if binary.BigEndian.Uint64(registers[i*8:]) != want {
			return fmt.Errorf("unsupported NAND controller registers")
		}
	}
	for name, want := range map[string]uint32{"reg": 0, "nand-ecc-strength": 8, "nand-ecc-step-size": 512} {
		value := nand.properties[name]
		if len(value) != 4 || binary.BigEndian.Uint32(value) != want {
			return fmt.Errorf("unsupported NAND parameter %s", name)
		}
	}
	if !bytes.Equal(nand.properties["nand-ecc-mode"], []byte("hw\x00")) {
		return fmt.Errorf("unsupported NAND ECC mode")
	}
	return nil
}

// The supported register layout is relative to a 2 MiB bus at 0xffe00000,
// beneath an identity-mapped SoC bus. Check the encoding and translation before
// comparing register offsets; identical reg bytes on another bus are unsafe.
func validateNANDBus(nodes []fdtNode, controller *fdtNode) error {
	cell := func(node *fdtNode, name string, want uint32) bool {
		value := node.properties[name]
		return len(value) == 4 && binary.BigEndian.Uint32(value) == want
	}
	bad := func() error { return fmt.Errorf("unsupported NAND bus addressing") }
	if !cell(controller, "#address-cells", 1) || !cell(controller, "#size-cells", 0) {
		return bad()
	}
	var translation []byte
	for _, value := range []uint64{0, 0xffe00000, 0x200000} {
		translation = binary.BigEndian.AppendUint64(translation, value)
	}
	path := controller.path
	for level := 0; level < 3; level++ {
		index := strings.LastIndex(path, "/")
		if index < 0 {
			return bad()
		}
		path = path[:index]
		var parent *fdtNode
		for i := range nodes {
			if nodes[i].path == path {
				parent = &nodes[i]
				break
			}
		}
		if parent == nil || !cell(parent, "#address-cells", 2) || !cell(parent, "#size-cells", 2) {
			return bad()
		}
		ranges, exists := parent.properties["ranges"]
		switch level {
		case 0:
			if !exists || !bytes.Equal(ranges, translation) {
				return bad()
			}
		case 1:
			if !exists || len(ranges) != 0 {
				return bad()
			}
		case 2:
			if path != "" {
				return bad()
			}
		}
	}
	return nil
}

func cstringListHas(data []byte, want string) bool {
	if len(data) == 0 || data[len(data)-1] != 0 {
		return false
	}
	for _, entry := range bytes.Split(data, []byte{0}) {
		if string(entry) == want {
			return true
		}
	}
	return false
}
