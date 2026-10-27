package amlogic

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"amlogicmini/internal/profile"
)

func TestAlpineEraseHasOneFixedAllowedRange(t *testing.T) {
	w := &wire{bulk: []bulkResult{{[]byte("success"), nil}}}
	if err := New(w, profile.A113X()).EraseAlpine(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(bytes.TrimRight(w.controls[0].data, "\x00")); got != "echo; nand erase 0x1200000 0xee00000" {
		t.Fatalf("erase=%q", got)
	}
}
func TestAlpineChunkRequiresMatchingRAMBeforeNANDWrite(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, 2048)
	w := writeReplies(1)
	w.responses = append(w.responses, make([]byte, 16))
	w.bulk = append(w.bulk, bulkResult{[]byte("success"), nil}, bulkResult{data, nil}, bulkResult{[]byte("success"), nil})
	if err := New(w, profile.A113X()).WriteAlpineChunk(context.Background(), "kernel", 0, data); err != nil {
		t.Fatal(err)
	}
	last := w.controls[len(w.controls)-1]
	if got := string(bytes.TrimRight(last.data, "\x00")); got != "echo; nand write 0x1080000 0x1200000 0x800" {
		t.Fatalf("write=%q", got)
	}
}
func TestAlpineRangeAndAlignmentFailuresNeverTouchUSB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset uint32
		size   int
	}{{"bootloader", 0, 2048}, {"kernel", 0x1900000, 2048}, {"dtb", 0xfffff, 2048}, {"ubi", 0xd400000, 2048}, {"kernel", 1, 2048}, {"ubi", 0, 1}, {"kernel", 0, 0x800800}} {
		w := writeReplies(1)
		err := New(w, profile.A113X()).WriteAlpineChunk(context.Background(), tc.name, tc.offset, make([]byte, tc.size))
		if err == nil || len(w.controls) != 0 {
			t.Fatalf("accepted %+v: %v", tc, err)
		}
	}
}
func TestBadRAMReadbackDoesNotIssueNANDWrite(t *testing.T) {
	w := writeReplies(1)
	w.responses = append(w.responses, make([]byte, 16))
	w.bulk = append(w.bulk, bulkResult{[]byte("success"), nil}, bulkResult{make([]byte, 2048), nil})
	if err := New(w, profile.A113X()).WriteAlpineChunk(context.Background(), "dtb", 0, bytes.Repeat([]byte{1}, 2048)); err == nil {
		t.Fatal("bad RAM accepted")
	}
	for _, p := range w.controls {
		if strings.Contains(string(p.data), "nand write") {
			t.Fatal("wrote unverified RAM")
		}
	}
}
func TestAlpineBootSplitsLongArgsAndChecksEveryExpectedValue(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{true: "wrong", false: "valid"}[wrong], func(t *testing.T) {
			w := &writeWire{wire: wire{responses: [][]byte{make([]byte, 16)}}}
			// Every command response is success; the final upload returns environment data.
			for i := 0; i < 40; i++ {
				w.bulk = append(w.bulk, bulkResult{[]byte("success"), nil})
			}
			env := make([]byte, 65536)
			args := "ubi.mtd=ubi root=ubi0:rootfs rootfstype=ubifs rootwait rw console=ttyAML0,115200n8 console=ttyGS0,115200 loglevel=8"
			if wrong {
				args = "wrong"
			}
			copy(env, "ba="+args+"\nka=setenv bootargs ${ba}\nkb=nand read 0x01000000 0x02b00000 0x00020000\nkc=nand read 0x01080000 0x01200000 0x00020000\nkd=booti 0x01080000 - 0x01000000\nbootcmd=run ka kb kc kd\nbootdelay=0\n")
			w.env = env
			ew := &envWire{writeWire: *w}
			err := New(ew, profile.A113X()).AlpineBoot(context.Background(), 64, 64)
			if (err != nil) != wrong {
				t.Fatalf("wrong=%v err=%v", wrong, err)
			}
			foundLiteral := false
			var words []string
			for _, p := range ew.controls {
				if p.request != 0x34 {
					continue
				}
				command := string(bytes.TrimRight(p.data, "\x00"))
				if len(command) > 63 {
					t.Fatal("oversized command")
				}
				if command == "echo; setenv ka 'setenv bootargs ${ba}'" {
					foundLiteral = true
				}
				if strings.HasPrefix(command, "echo; setenv ba ") {
					v := strings.TrimPrefix(command, "echo; setenv ba ")
					v = strings.TrimPrefix(v, "${ba} ")
					words = append(words, v)
				}
			}
			expected := "ubi.mtd=ubi root=ubi0:rootfs rootfstype=ubifs rootwait rw console=ttyAML0,115200n8 console=ttyGS0,115200 loglevel=8"
			if !foundLiteral || strings.Join(words, " ") != expected {
				t.Fatalf("boot args commands=%q literal=%v", words, foundLiteral)
			}
		})
	}
}

// Return environment bytes only after upload metadata was requested.
type envWire struct {
	writeWire
	reading bool
}

func (w *envWire) Control(ctx context.Context, k, r byte, v, i uint16, b []byte) (int, error) {
	if r == 0x33 {
		w.reading = true
	}
	return w.writeWire.Control(ctx, k, r, v, i, b)
}
func (w *envWire) ReadBulk(ctx context.Context, b []byte) (int, error) {
	if w.reading {
		return copy(b, w.env), nil
	}
	return w.writeWire.ReadBulk(ctx, b)
}

func TestAlpineBootRejectsOverlappingDTBWithoutUSB(t *testing.T) {
	w := &writeWire{}
	if err := New(w, profile.A113X()).AlpineBoot(context.Background(), 64, 0x80001); err == nil || len(w.controls) != 0 {
		t.Fatal("DTB RAM overlap accepted")
	}
}
func TestBootImageVerificationUsesOneContinuousNANDRead(t *testing.T) {
	w := &wire{responses: [][]byte{make([]byte, 16)}, bulk: []bulkResult{{[]byte("success"), nil}, {[]byte("success"), nil}, {[]byte("success"), nil}, {make([]byte, 64), nil}}}
	var out bytes.Buffer
	n, err := New(w, profile.A113X()).ReadAlpineBootImage(context.Background(), "kernel", 64, &out)
	if err != nil || n != 64 {
		t.Fatalf("read=%d err=%v", n, err)
	}
	var reads []string
	for _, p := range w.controls {
		command := string(bytes.TrimRight(p.data, "\x00"))
		if strings.Contains(command, "nand read ") {
			reads = append(reads, command)
		}
	}
	if len(reads) != 1 || reads[0] != "echo; nand read 0x1080000 0x1200000 0x20000" {
		t.Fatal(reads)
	}
}

func TestLargeBootKernelUsesOneNANDReadThenStreamsRAM(t *testing.T) {
	w := &wire{}
	for i := 0; i < 3; i++ {
		w.bulk = append(w.bulk, bulkResult{[]byte("success"), nil})
	}
	page := make([]byte, 65536)
	for i := 0; i < 256; i++ {
		w.responses = append(w.responses, make([]byte, 16))
		w.bulk = append(w.bulk, bulkResult{page, nil})
	}
	n, err := New(w, profile.A113X()).ReadAlpineBootImage(context.Background(), "kernel", 0x1000000, io.Discard)
	if err != nil || n != 0x1000000 {
		t.Fatalf("read=%d err=%v", n, err)
	}
	reads := 0
	for _, p := range w.controls {
		if strings.Contains(string(p.data), "nand read ") {
			reads++
			if string(bytes.TrimRight(p.data, "\x00")) != "echo; nand read 0x1080000 0x1200000 0x1000000" {
				t.Fatal(string(p.data))
			}
		}
	}
	if reads != 1 {
		t.Fatalf("boot read split into %d NAND commands", reads)
	}
}
