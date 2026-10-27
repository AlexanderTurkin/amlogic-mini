package usb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"amlogicmini/internal/amlogic"
	"amlogicmini/internal/profile"
	"amlogicmini/internal/restore"
)

func TestHardwareBootloaderRead(t *testing.T) {
	if os.Getenv("AMLOGIC_MINI_HARDWARE") != "1" {
		t.Skip("requires a physically connected device")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	t.Log("Connect the device to the computer using a USB data cable now")
	connection, version, err := Open(ctx, 115*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	}()
	t.Logf("Native Go IDENTIFY: %x", version)
	c := amlogic.New(connection, profile.A113X())
	if err := c.Prime(ctx); err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	for _, out := range []*bytes.Buffer{&first, &second} {
		n, err := c.ReadPartition(ctx, "bootloader", 0x200000, out)
		if err != nil || n != 0x200000 {
			t.Fatalf("read bootloader: %d bytes: %v", n, err)
		}
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("two bootloader reads differ")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(first.Bytes()))
	if want := os.Getenv("AMLOGIC_MINI_BOOT_SHA256"); want != "" && hash != want {
		t.Fatalf("bootloader checksum %s, expected %s", hash, want)
	}
	t.Logf("Bootloader: 2097152 bytes; two identical native reads; SHA-256 %s", hash)
}

func TestHardwareRestoreRAMRoundTrip(t *testing.T) {
	dir := os.Getenv("AMLOGIC_MINI_RESTORE_BACKUP")
	if dir == "" {
		t.Skip("requires explicit backup path and a physically connected device")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	session, err := restore.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	t.Log("Reconnect the device now. Only keys/environment reads and scratch RAM writes will run.")
	connection, _, err := Open(ctx, 170*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	check, err := session.CheckDevice(ctx, amlogic.New(connection, profile.A113X()))
	if err != nil {
		t.Fatal(err)
	}
	if !check.RAMVerified {
		t.Fatal("RAM test was not verified")
	}
	t.Log("Device fingerprint matched. Native 67584-byte RAM upload/readback verified; no NAND write performed.")
}
