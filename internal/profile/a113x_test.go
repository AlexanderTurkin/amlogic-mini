package profile

import "testing"

func TestNeutralHardwareProfileLookup(t *testing.T) {
	p, ok := Lookup("amlogic-a113x-nand256")
	if !ok || p.Name != "amlogic-a113x-nand256" || p.Geometry.NANDSize != 0x10000000 || p.Validate() != nil {
		t.Fatalf("neutral hardware profile unavailable: %+v", p)
	}
}
