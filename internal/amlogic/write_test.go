package amlogic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"amlogicmini/internal/profile"
)

type writeWire struct {
	env []byte
	wire
	sent   [][]byte
	failAt int
	short  bool
}

func (w *writeWire) WriteBulk(_ context.Context, b []byte) (int, error) {
	w.sent = append(w.sent, bytes.Clone(b))
	if w.short || (w.failAt > 0 && len(w.sent) == w.failAt) {
		return len(b) - 1, nil
	}
	return len(b), nil
}
func writeReplies(chunks int) *writeWire {
	w := &writeWire{}
	w.responses = [][]byte{append([]byte("success"), make([]byte, 57)...)}
	for i := 0; i < chunks; i++ {
		w.bulk = append(w.bulk, bulkResult{[]byte("OK!!"), nil})
	}
	w.bulk = append(w.bulk, bulkResult{[]byte("success"), nil})
	return w
}
func TestWriteRAMUsesVendorDownloadMediaPackets(t *testing.T) {
	w := writeReplies(1)
	c := New(w, profile.A113X())
	if err := c.WriteRAM(context.Background(), 0x01080000, []byte{1, 2, 3, 4, 5, 6, 7}); err != nil {
		t.Fatal(err)
	}
	if len(w.controls) != 4 {
		t.Fatalf("controls=%d", len(w.controls))
	}
	a := w.controls[0]
	if a.kind != 0x40 || a.request != 0x30 || a.value != 0 || a.index != 1 || len(a.data) != 64 || string(bytes.TrimRight(a.data, "\x00")) != "download mem 0x1080000 normal 0x7" {
		t.Fatalf("download=%+v", a)
	}
	a = w.controls[1]
	if a.kind != 0xc0 || a.request != 0x31 || a.value != 0 || a.index != 0 || len(a.data) != 64 {
		t.Fatalf("initial status=%+v", a)
	}
	a = w.controls[2]
	want := []byte{0, 0, 0, 0, 7, 0, 0, 0, 0, 0, 0, 0, 6, 8, 10, 4, 239, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if a.kind != 0x40 || a.request != 0x32 || a.value != 1 || a.index != 65535 || !bytes.Equal(a.data, want) {
		t.Fatalf("media header=%+v", a)
	}
	if len(w.sent) != 1 || !bytes.Equal(w.sent[0], []byte{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("payload=%v", w.sent)
	}
	if got := string(bytes.TrimRight(w.controls[3].data, "\x00")); got != "download get_status" {
		t.Fatalf("finish=%q", got)
	}
}
func TestWriteMediaSequenceAcross64KiBBoundary(t *testing.T) {
	w := writeReplies(2)
	data := make([]byte, 65539)
	for i := range data {
		data[i] = byte(i*37 + 19)
	}
	if err := New(w, profile.A113X()).WriteRAM(context.Background(), 0x1080000, data); err != nil {
		t.Fatal(err)
	}
	if len(w.sent) != 2 || len(w.sent[0]) != 65536 || len(w.sent[1]) != 3 {
		t.Fatalf("chunks=%v", len(w.sent))
	}
	if !bytes.Equal(w.sent[0], data[:65536]) || !bytes.Equal(w.sent[1], data[65536:]) {
		t.Fatal("reused buffer corrupted packet contents")
	}
	a := w.controls[3].data
	if a[4] != 3 || a[8] != 16 {
		t.Fatalf("second header=%x", a)
	}
}
func TestRestoreWriterRejectsProtectedNamesAndBadSizesWithoutUSB(t *testing.T) {
	for _, name := range []string{"bootloader", "tpl", "misc", "keys", "data; reset", "system_a"} {
		w := writeReplies(1)
		c := New(w, profile.A113X())
		if err := c.WritePartition(context.Background(), name, 1, strings.NewReader("x"), nil); err == nil {
			t.Fatalf("accepted %q", name)
		}
		if len(w.controls) != 0 {
			t.Fatal("invalid input touched USB")
		}
	}
}
func TestWritePartitionUsesNamedStoreAndStopsOnShortWrite(t *testing.T) {
	w := writeReplies(1)
	w.short = true
	err := New(w, profile.A113X()).WritePartition(context.Background(), "boot_a", 2048, bytes.NewReader(make([]byte, 2048)), nil)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write=%v", err)
	}
	if got := string(bytes.TrimRight(w.controls[0].data, "\x00")); got != "download store boot_a normal 0x800" {
		t.Fatalf("command=%q", got)
	}
	if len(w.controls) != 3 {
		t.Fatal("continued after failed write")
	}
}
func TestWriteStopsOnFailedOrEmptyAcknowledgement(t *testing.T) {
	for _, reply := range []string{"", "ERR: write failed", "OK", "Continue:32"} {
		w := writeReplies(1)
		w.bulk = []bulkResult{{[]byte(reply), nil}}
		if err := New(w, profile.A113X()).WriteRAM(context.Background(), 0x1080000, []byte{1}); err == nil {
			t.Fatalf("accepted ack %q", reply)
		}
		if len(w.controls) != 3 {
			t.Fatal("continued after failed acknowledgement")
		}
	}
}
func TestWriteCanceledBeforeDownloadDoesNotTouchUSB(t *testing.T) {
	w := writeReplies(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(w, profile.A113X()).WriteRAM(ctx, 0x1080000, []byte{1})
	if !errors.Is(err, context.Canceled) || len(w.controls) != 0 {
		t.Fatalf("canceled=%v controls=%d", err, len(w.controls))
	}
}
func TestStockBootStopsIfSaveFailsAndVerifiesEnvironment(t *testing.T) {
	w := &wire{bulk: []bulkResult{{[]byte("failed"), nil}}}
	if err := New(w, profile.A113X()).StockBoot(context.Background()); err == nil || len(w.controls) != 1 {
		t.Fatal("stock boot failure ignored")
	}
}

func TestWriteFailureIncludesBoundedStatusForDiagnosis(t *testing.T) {
	w := writeReplies(1)
	w.bulk = []bulkResult{{[]byte("WRITE FAILED 17"), nil}}
	err := New(w, profile.A113X()).WriteRAM(context.Background(), 0x1080000, []byte{1})
	if err == nil || !strings.Contains(err.Error(), "WRITE FAILED 17") {
		t.Fatalf("missing actual status: %v", err)
	}
}

func TestMediaAcknowledgementAllowsFirmwareBufferTail(t *testing.T) {
	w := writeReplies(1)
	w.bulk[0].data = []byte("OK!!ess")
	if err := New(w, profile.A113X()).WriteRAM(context.Background(), 0x1080000, []byte{1}); err != nil {
		t.Fatalf("vendor OK!! prefix rejected: %v", err)
	}
}

func TestStockBootVerifiesEnvironmentAfterSave(t *testing.T) {
	for _, wantMatch := range []bool{false, true} {
		t.Run(fmt.Sprint(wantMatch), func(t *testing.T) {
			w := &wire{responses: [][]byte{make([]byte, 16)}}
			for i := 0; i < 5; i++ {
				w.bulk = append(w.bulk, bulkResult{[]byte("success"), nil})
			}
			env := make([]byte, 65536)
			if wantMatch {
				copy(env, "bootcmd=run storeboot\n")
			} else {
				copy(env, "bootcmd=run ka kb kc kd\n")
			}
			w.bulk = append(w.bulk, bulkResult{env, nil})
			err := New(w, profile.A113X()).StockBoot(context.Background())
			if (err == nil) != wantMatch {
				t.Fatalf("environment match=%v err=%v", wantMatch, err)
			}
			for i, want := range []string{"echo; setenv bootcmd 'run storeboot'", "echo; saveenv"} {
				if got := string(bytes.TrimRight(w.controls[i].data, "\x00")); got != want {
					t.Fatalf("command=%s", got)
				}
			}
		})
	}
}

func TestPartitionProgressOnlyReportsAcknowledgedBytes(t *testing.T) {
	for _, mode := range []string{"success", "last packet failed", "final status failed"} {
		t.Run(mode, func(t *testing.T) {
			w := writeReplies(2)
			if mode == "last packet failed" {
				w.failAt = 2
			}
			if mode == "final status failed" {
				w.bulk[2] = bulkResult{[]byte("write failed"), nil}
			}
			var measured []uint32
			err := New(w, profile.A113X()).WritePartition(context.Background(), "boot_a", 131072, bytes.NewReader(make([]byte, 131072)), func(done uint32) { measured = append(measured, done) })
			if (err == nil) != (mode == "success") {
				t.Fatalf("result: %v", err)
			}
			want := []uint32{65536}
			if mode == "success" {
				want = append(want, 131072)
			}
			if !reflect.DeepEqual(measured, want) {
				t.Fatalf("unacknowledged progress: %v, want %v", measured, want)
			}
		})
	}
}
