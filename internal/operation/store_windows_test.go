//go:build windows

package operation

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReportOnDifferentWindowsVolumeIsOutsideSources(t *testing.T) {
	report := filepath.Join(t.TempDir(), "operation.json")
	other := "X:"
	if strings.EqualFold(filepath.VolumeName(report), other) {
		other = "Y:"
	}
	source := other + `\amlogic-mini\backups\stock`
	if _, _, err := reportLocation(report, source); err != nil {
		t.Fatalf("different-volume report was rejected: %v", err)
	}
}
