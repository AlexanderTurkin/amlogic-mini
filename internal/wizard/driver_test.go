package wizard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type driverStub struct {
	ready                bool
	checkErr, installErr error
	checks, installs     int
}

func (d *driverStub) Ready(context.Context) (bool, error) { d.checks++; return d.ready, d.checkErr }
func (d *driverStub) Install(context.Context) error       { d.installs++; return d.installErr }

func TestUSBDriverPreparationPrecedesConnection(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		ready                bool
		answer               string
		checkErr, installErr error
		opens, installs      int
	}{
		{name: "ready", ready: true, opens: 1},
		{name: "declined", answer: "0"},
		{name: "default-no", answer: ""},
		{name: "accepted", answer: "1", opens: 1, installs: 1},
		{name: "check-error", checkErr: errors.New("check failed")},
		{name: "install-error", answer: "1", installErr: errors.New("install failed"), installs: 1},
		{name: "UAC-cancel", answer: "1", installErr: errors.New("UAC cancelled"), installs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, out, opens := options(t, tc.answer)
			d := &driverStub{ready: tc.ready, checkErr: tc.checkErr, installErr: tc.installErr}
			o.USBDriver = d
			_, _, _, err := newWizard(o).connect(context.Background())
			if err == nil {
				t.Fatal("fixture must end before real USB work")
			}
			if *opens != tc.opens || d.installs != tc.installs {
				t.Fatalf("opened %d, installed %d", *opens, d.installs)
			}
			if !tc.ready && tc.checkErr == nil && (!strings.Contains(out.String(), "сертификат") || !strings.Contains(out.String(), "1b8e:c003")) {
				t.Fatal("missing driver consent details")
			}
		})
	}
}
func TestCancelledDriverPreparationNeverOpensUSB(t *testing.T) {
	o, _, opens := options(t, "1")
	d := &driverStub{}
	o.USBDriver = d
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := newWizard(o).connect(ctx)
	if !errors.Is(err, context.Canceled) || *opens != 0 || d.installs != 0 {
		t.Fatalf("cancelled: %v, opens %d, installs %d", err, *opens, d.installs)
	}
}
