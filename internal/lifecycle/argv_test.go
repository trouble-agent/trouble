package lifecycle

import (
	"testing"
)

func TestScanProcCmdlineCurrent(t *testing.T) {
	// The test process argv does not contain secrets; scan should pass.
	if err := ScanProcCmdline(); err != nil {
		t.Fatalf("ScanProcCmdline: %v", err)
	}
}
