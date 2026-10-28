// Command usbprobe prints USB descriptors and identify replies for Amlogic
// boards. It is a diagnostic helper: it never writes to the device.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/gousb"
)

const amlogicVendor = 0x1b8e

type deviceInfo struct {
	vendor, product    uint16
	bus, address, port int
	class              int
	speed              string
	path               []int
}

func main() {
	debug := flag.Int("debug", 0, "libusb debug level (0-4)")
	timeout := flag.Duration("timeout", 5*time.Second, "control transfer timeout")
	lengthsFlag := flag.String("lengths", "4,7,8", "comma-separated identify buffer lengths")
	vid := flag.Int("vid", amlogicVendor, "vendor ID to probe (0x1b8e by default)")
	identify := flag.Bool("identify", true, "send the identify request to matching devices")
	resetFirst := flag.Bool("reset-first", false, "reset the device before identify")
	configFirst := flag.Bool("config-first", false, "set configuration 1 before identify")
	claim := flag.Bool("claim", true, "claim configuration 1 / interface 0 and list endpoints")
	hold := flag.Duration("hold", 0, "keep the device open for this long after probing")
	wait := flag.Bool("wait", false, "wait for the device and probe it as soon as it appears")
	waitFor := flag.Duration("wait-for", 2*time.Minute, "how long to wait with -wait")
	flag.Parse()

	lengths, err := parseLengths(*lengthsFlag)
	if err != nil {
		fatal(err)
	}

	ctx := gousb.NewContext()
	defer ctx.Close()
	if *debug > 0 {
		ctx.Debug(*debug)
	}

	if *wait {
		waitAndProbe(ctx, uint16(*vid), *waitFor, lengths, *resetFirst, *configFirst, *claim, *hold, *timeout)
		return
	}

	var found []deviceInfo
	if _, err := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		found = append(found, deviceInfo{
			vendor:  uint16(d.Vendor),
			product: uint16(d.Product),
			bus:     d.Bus,
			address: d.Address,
			port:    d.Port,
			class:   int(d.Class),
			speed:   d.Speed.String(),
			path:    append([]int(nil), d.Path...),
		})
		return false
	}); err != nil {
		fmt.Fprintf(os.Stderr, "enumerate: %v\n", err)
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].bus != found[j].bus {
			return found[i].bus < found[j].bus
		}
		return found[i].address < found[j].address
	})
	fmt.Printf("USB devices: %d\n", len(found))
	for _, d := range found {
		fmt.Printf("  %04x:%04x  bus %d  addr %d  port %d  path %v  class %d  speed %s\n",
			d.vendor, d.product, d.bus, d.address, d.port, d.path, d.class, d.speed)
	}
	if len(found) == 0 {
		fmt.Println("no USB devices visible: check the cable, board power and USB download mode")
		return
	}

	if !*identify {
		return
	}

	devices, err := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		return uint16(d.Vendor) == uint16(*vid)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open Amlogic devices: %v\n", err)
	}
	defer func() {
		for _, d := range devices {
			d.Close()
		}
	}()
	if len(devices) == 0 {
		fmt.Println("no matching device for the selected VID; if the board is listed above with another VID/PID, send this output")
		return
	}

	for _, dev := range devices {
		probeDevice(dev, lengths, *resetFirst, *configFirst, *claim, *hold, *timeout)
	}
}

func waitAndProbe(ctx *gousb.Context, vid uint16, wait time.Duration, lengths []int, resetFirst, configFirst, claim bool, hold, timeout time.Duration) {
	start := time.Now()
	deadline := start.Add(wait)
	fmt.Printf("waiting for %04x for %s...\n", vid, wait)
	for time.Now().Before(deadline) {
		devices, err := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
			return uint16(d.Vendor) == vid
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "open: %v\n", err)
		}
		if len(devices) > 0 {
			fmt.Printf("found %04x after %s\n", vid, time.Since(start).Round(time.Millisecond))
			for _, dev := range devices {
				probeDevice(dev, lengths, resetFirst, configFirst, claim, hold, timeout)
				dev.Close()
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Println("timeout")
}

func probeDevice(dev *gousb.Device, lengths []int, resetFirst, configFirst, claim bool, hold, timeout time.Duration) {
	dev.ControlTimeout = timeout
	_ = dev.SetAutoDetach(true)
	product, _ := dev.Product()
	manufacturer, _ := dev.Manufacturer()
	serial, _ := dev.SerialNumber()
	fmt.Printf("\nAmlogic device %04x:%04x bus %d addr %d product=%q manufacturer=%q serial=%q\n",
		uint16(dev.Desc.Vendor), uint16(dev.Desc.Product), dev.Desc.Bus, dev.Desc.Address,
		product, manufacturer, serial)
	fmt.Printf("  spec=%#04x device=%#04x class=%s\n",
		uint16(dev.Desc.Spec), uint16(dev.Desc.Device), dev.Desc.Class)
	printDescriptors(dev.Desc)

	if active, err := dev.ActiveConfigNum(); err != nil {
		fmt.Printf("  active config: unknown (%v)\n", err)
	} else {
		fmt.Printf("  active config: %d\n", active)
	}

	var cfg *gousb.Config
	if resetFirst {
		fmt.Printf("  usb reset: %v\n", dev.Reset())
		time.Sleep(500 * time.Millisecond)
	}
	if configFirst {
		var err error
		cfg, err = dev.Config(1)
		if err != nil {
			fmt.Printf("  set configuration 1: %v\n", err)
		} else {
			fmt.Printf("  set configuration 1: ok\n")
		}
	}

	for _, n := range lengths {
		buf := make([]byte, n)
		got, err := dev.Control(0xc0, 0x20, 0, 0, buf)
		shown := buf
		if got >= 0 && got < len(buf) {
			shown = buf[:got]
		}
		fmt.Printf("  identify len=%d -> n=%d data=% x err=%v\n", n, got, shown, err)
	}

	if claim {
		if cfg == nil {
			var err error
			cfg, err = dev.Config(1)
			if err != nil {
				fmt.Printf("  claim config 1: %v\n", err)
			}
		}
		if cfg != nil {
			probeInterface(cfg)
			cfg.Close()
		}
	} else if cfg != nil {
		cfg.Close()
	}
	if hold > 0 {
		fmt.Printf("  holding device open for %s\n", hold)
		time.Sleep(hold)
	}
}

func printDescriptors(d *gousb.DeviceDesc) {
	for _, cfg := range d.Configs {
		fmt.Printf("  config %d: self-powered=%t remote-wakeup=%t max-power=%dmA interfaces=%d\n",
			cfg.Number, cfg.SelfPowered, cfg.RemoteWakeup, int(cfg.MaxPower), len(cfg.Interfaces))
		for _, intf := range cfg.Interfaces {
			for _, alt := range intf.AltSettings {
				fmt.Printf("    interface %d alt %d class=%s subclass=%s protocol=%s endpoints=%d\n",
					alt.Number, alt.Alternate, alt.Class, alt.SubClass, alt.Protocol, len(alt.Endpoints))
				for _, ep := range alt.Endpoints {
					fmt.Printf("      endpoint %s number=%d direction=%s type=%s max-packet=%d interval=%s\n",
						ep.Address, ep.Number, ep.Direction, ep.TransferType, ep.MaxPacketSize, ep.PollInterval)
				}
			}
		}
	}
}

func probeInterface(cfg *gousb.Config) {
	fmt.Printf("  claimed config %d\n", cfg.Desc.Number)

	intf, err := cfg.Interface(0, 0)
	if err != nil {
		fmt.Printf("  claim interface 0/0: %v\n", err)
		return
	}
	defer intf.Close()
	fmt.Printf("  claimed interface 0 alt 0: %s\n", intf.Setting)

	if ep, err := intf.InEndpoint(1); err != nil {
		fmt.Printf("  IN endpoint 1: %v\n", err)
	} else {
		fmt.Printf("  IN endpoint 1: %s\n", ep)
	}
	if ep, err := intf.OutEndpoint(2); err != nil {
		fmt.Printf("  OUT endpoint 2: %v\n", err)
	} else {
		fmt.Printf("  OUT endpoint 2: %s\n", ep)
	}
}

func parseLengths(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 8 {
			return nil, fmt.Errorf("invalid identify length %q", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no identify lengths")
	}
	return out, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "usbprobe:", err)
	os.Exit(1)
}
