package amlogic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"amlogicmini/internal/profile"
)

const alpineArgs = "ubi.mtd=ubi root=ubi0:rootfs rootfstype=ubifs rootwait rw console=ttyAML0,115200n8 console=ttyGS0,115200 loglevel=8"

func (c *Client) checkAlpineProfile() error {
	if !reflect.DeepEqual(c.profile, profile.A113X()) {
		return fmt.Errorf("flashing Alpine requires the exact A113X profile")
	}
	return nil
}
func (c *Client) EraseAlpine(ctx context.Context) error {
	if err := c.checkAlpineProfile(); err != nil {
		return err
	}
	return c.commandWithStatus(ctx, "echo; nand erase 0x1200000 0xee00000", 120*time.Second, c.readWriteStatus)
}

// WriteAlpineChunk cannot address the boot chain or reserved NAND. It checks the
// staged RAM before writing; final image verification belongs to the flasher.
func (c *Client) WriteAlpineChunk(ctx context.Context, name string, offset uint32, data []byte) error {
	if err := c.checkAlpineProfile(); err != nil {
		return err
	}
	var region profile.Region
	for _, r := range profile.AlpineRegions() {
		if r.Name == name {
			region = r
		}
	}
	if region.Size == 0 || len(data) == 0 || len(data) > 0x800000 || len(data)%2048 != 0 || offset%0x20000 != 0 || uint64(offset)+uint64(len(data)) > uint64(region.Size) {
		return fmt.Errorf("write outside allowed Alpine region or alignment: %q", name)
	}
	if err := c.WriteRAM(ctx, 0x1080000, data); err != nil {
		return err
	}
	readback, err := c.ReadRAM(ctx, 0x1080000, uint32(len(data)))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, readback) {
		return fmt.Errorf("RAM staging mismatch; NAND chunk was not written")
	}
	command := fmt.Sprintf("echo; nand write 0x1080000 0x%x 0x%x", region.Offset+offset, len(data))
	return c.commandWithStatus(ctx, command, 120*time.Second, c.readWriteStatus)
}
func (c *Client) AlpineBoot(ctx context.Context, kernelSize, dtbSize uint32) error {
	if err := c.checkAlpineProfile(); err != nil {
		return err
	}
	if kernelSize < 64 || kernelSize > 0x1900000 || dtbSize < 40 || dtbSize > 0x80000 {
		return fmt.Errorf("invalid Alpine boot image lengths")
	}
	round := func(v uint32) uint32 { return (v + 0x1ffff) / 0x20000 * 0x20000 }
	expected := [][2]string{
		{"ka", "setenv bootargs ${ba}"},
		{"kb", fmt.Sprintf("nand read 0x01000000 0x02b00000 0x%08x", round(dtbSize))},
		{"kc", fmt.Sprintf("nand read 0x01080000 0x01200000 0x%08x", round(kernelSize))},
		{"kd", "booti 0x01080000 - 0x01000000"},
	}
	for _, entry := range expected {
		if err := c.commandWithStatus(ctx, fmt.Sprintf("echo; setenv %s '%s'", entry[0], entry[1]), 60*time.Second, c.readWriteStatus); err != nil {
			return err
		}
	}
	if err := c.setAlpineArgs(ctx); err != nil {
		return err
	}
	expected = append(expected, [2]string{"ba", alpineArgs}, [2]string{"bootdelay", "0"}, [2]string{"bootcmd", "run ka kb kc kd"})
	for _, entry := range expected[len(expected)-2:] {
		if err := c.commandWithStatus(ctx, fmt.Sprintf("echo; setenv %s '%s'", entry[0], entry[1]), 60*time.Second, c.readWriteStatus); err != nil {
			return err
		}
	}
	if err := c.commandWithStatus(ctx, "echo; saveenv", 60*time.Second, c.readWriteStatus); err != nil {
		return err
	}
	env, err := c.Environment(ctx)
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(env), "\x00"), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			if _, duplicate := values[name]; duplicate {
				return fmt.Errorf("duplicate environment variable %s", name)
			}
			values[name] = value
		}
	}
	for _, entry := range expected {
		if values[entry[0]] != entry[1] {
			return fmt.Errorf("readback mismatch in Alpine environment: %s", entry[0])
		}
	}
	return nil
}
func (c *Client) setAlpineArgs(ctx context.Context) error {
	prefix := "echo; setenv ba "
	var pending []string
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		return c.commandWithStatus(ctx, prefix+strings.Join(pending, " "), 60*time.Second, c.readWriteStatus)
	}
	for _, word := range strings.Fields(alpineArgs) {
		candidate := append(append([]string(nil), pending...), word)
		if len(prefix)+len(strings.Join(candidate, " ")) > 63 {
			if err := flush(); err != nil {
				return err
			}
			prefix = "echo; setenv ba ${ba} "
			pending = nil
		}
		if len(prefix)+len(word) > 63 {
			return fmt.Errorf("boot argument for Alpine exceeds command payload limit")
		}
		pending = append(pending, word)
	}
	return flush()
}

// ReadAlpineBootImage reproduces the exact single NAND read used by bootcmd.
// Staging up to 25 MiB for the kernel is the same RAM range that booti uses;
// callers cannot select arbitrary addresses or extend the read-only backup API.
func (c *Client) ReadAlpineBootImage(ctx context.Context, name string, size uint32, out io.Writer) (int64, error) {
	if err := c.checkAlpineProfile(); err != nil {
		return 0, err
	}
	var ram, offset, limit uint32
	switch name {
	case "kernel":
		ram, offset, limit = 0x1080000, 0x1200000, 0x1900000
	case "dtb":
		ram, offset, limit = 0x1000000, 0x2b00000, 0x80000
	default:
		return 0, fmt.Errorf("not an Alpine boot image")
	}
	if size == 0 || size > limit || out == nil {
		return 0, fmt.Errorf("boot image read outside bounds")
	}
	rounded := (size + 0x1ffff) / 0x20000 * 0x20000
	for _, command := range []string{fmt.Sprintf("echo; mw.b 0x%x 0xa5 0x%x", ram, rounded), fmt.Sprintf("echo; nand read 0x%x 0x%x 0x%x", ram, offset, rounded)} {
		if err := c.commandWithStatus(ctx, command, 120*time.Second, c.readWriteStatus); err != nil {
			return 0, err
		}
	}
	if err := c.command(ctx, fmt.Sprintf("upload mem 0x%x normal 0x%x", ram, size)); err != nil {
		return 0, err
	}
	return c.receive(ctx, size, out)
}
