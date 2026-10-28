package amlogic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"amlogicmini/internal/profile"
)

type packet struct {
	kind, request byte
	value, index  uint16
	data          []byte
}
type bulkResult struct {
	data []byte
	err  error
}
type wire struct {
	controls  []packet
	responses [][]byte
	bulk      []bulkResult
}

func (w *wire) Control(_ context.Context, k, r byte, v, i uint16, b []byte) (int, error) {
	w.controls = append(w.controls, packet{k, r, v, i, bytes.Clone(b)})
	if k&0x80 != 0 {
		if len(w.responses) == 0 {
			return 0, io.EOF
		}
		x := w.responses[0]
		w.responses = w.responses[1:]
		return copy(b, x), nil
	}
	return len(b), nil
}
func (w *wire) ReadBulk(_ context.Context, b []byte) (int, error) {
	if len(w.bulk) == 0 {
		return 0, io.EOF
	}
	r := w.bulk[0]
	w.bulk = w.bulk[1:]
	return copy(b, r.data), r.err
}

func TestIdentifyRejectsMalformedReplies(t *testing.T) {
	for _, reply := range [][]byte{nil, {0, 0, 0, 0}, {0, 0, 0, 0, 0, 0, 0}} {
		w := &wire{responses: [][]byte{reply}}
		if _, err := New(w, profile.A113X()).Identify(context.Background()); err == nil {
			t.Fatalf("accepted %x", reply)
		}
	}
}
func TestIdentifyAcceptsShortVendorReply(t *testing.T) {
	w := &wire{responses: [][]byte{{0, 7, 0, 16}}}
	v, err := New(w, profile.A113X()).Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != [7]byte{0, 7, 0, 16, 0, 0, 0} {
		t.Fatalf("version: %x", v)
	}
}
func TestIdentifyUsesReadOnlyVendorRequest(t *testing.T) {
	w := &wire{responses: [][]byte{{0, 7, 0, 16, 0, 0, 0}}}
	v, err := New(w, profile.A113X()).Identify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != [7]byte{0, 7, 0, 16, 0, 0, 0} {
		t.Fatalf("version: %x", v)
	}
	p := w.controls[0]
	if p.kind != 0xc0 || p.request != 0x20 || p.value != 0 || p.index != 0 || len(p.data) != 7 {
		t.Fatalf("wrong identify packet: %+v", p)
	}
}

func TestCanceledIdentifyDoesNotTouchUSB(t *testing.T) {
	w := &wire{responses: [][]byte{{0, 7, 0, 16, 0, 0, 0}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(w, profile.A113X()).Identify(ctx)
	if !errors.Is(err, context.Canceled) || len(w.controls) != 0 {
		t.Fatalf("canceled identify touched USB: %v", err)
	}
}
func TestReadRAMUsesUploadAndReadMedia(t *testing.T) {
	w := &wire{responses: [][]byte{make([]byte, 16)}, bulk: []bulkResult{{[]byte("success"), nil}, {[]byte("abc"), nil}}}
	b, err := New(w, profile.A113X()).ReadRAM(context.Background(), 0x01080000, 3)
	if err != nil || string(b) != "abc" {
		t.Fatalf("read: %q %v", b, err)
	}
	cmd := w.controls[0]
	want := make([]byte, 64)
	copy(want, "upload mem 0x1080000 normal 0x3")
	if cmd.kind != 0x40 || cmd.request != 0x34 || cmd.index != 2 || !bytes.Equal(cmd.data, want) {
		t.Fatalf("upload packet: %+v", cmd)
	}
	r := w.controls[1]
	if r.kind != 0xc0 || r.request != 0x33 || r.value != 4096 || r.index != 1 || len(r.data) != 16 {
		t.Fatalf("media request: %+v", r)
	}
}
func TestReadPartitionPreservesBytesOnDisconnect(t *testing.T) {
	w := &wire{responses: [][]byte{make([]byte, 16)}, bulk: []bulkResult{{[]byte("success"), nil}, {[]byte("ab"), io.ErrUnexpectedEOF}}}
	var out bytes.Buffer
	n, err := New(w, profile.A113X()).ReadPartition(context.Background(), "bootloader", 0x200000, &out)
	if n != 2 || out.String() != "ab" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost partial data: %d %q %v", n, out.String(), err)
	}
}
func TestInvalidRangesNeverReachUSB(t *testing.T) {
	w := &wire{}
	c := New(w, profile.A113X())
	ctx := context.Background()
	if _, e := c.ReadNAND(ctx, 0x10000000, 2048, false); e == nil {
		t.Fatal("NAND overflow accepted")
	}
	if _, e := c.ReadRAM(ctx, 0xffffffff, 64); e == nil {
		t.Fatal("RAM overflow accepted")
	}
	if _, e := c.ReadPartition(ctx, "bootloader; nand erase.chip", 1, io.Discard); e == nil {
		t.Fatal("command injection accepted")
	}
	if len(w.controls) != 0 {
		t.Fatal("invalid operation touched USB")
	}
}
func TestFailedCommandStopsBeforeReadingMedia(t *testing.T) {
	w := &wire{bulk: []bulkResult{{[]byte("failed: bad request"), nil}}}
	if _, err := New(w, profile.A113X()).ReadRAM(context.Background(), 0x01080000, 64); err == nil {
		t.Fatal("failed command accepted")
	}
	if len(w.controls) != 1 {
		t.Fatal("read after command failure")
	}
}

func TestReceivePreservesSplitPacketsAndUnalignedTail(t *testing.T) {
	parts := [][]byte{bytes.Repeat([]byte{0x11}, 17), bytes.Repeat([]byte{0x22}, 65519), bytes.Repeat([]byte{0x33}, 513)}
	w := &wire{responses: [][]byte{make([]byte, 16), make([]byte, 16)}}
	for _, part := range parts {
		w.bulk = append(w.bulk, bulkResult{data: part})
	}
	want := bytes.Join(parts, nil)
	var out bytes.Buffer
	n, err := New(w, profile.A113X()).receive(context.Background(), uint32(len(want)), &out)
	if err != nil || n != int64(len(want)) || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("split transfer corrupted: n=%d err=%v", n, err)
	}
}
