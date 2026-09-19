//go:build windows

package sensors

import "os"

// findProcess is the single seam used by the Windows process-group helpers.
func findProcess(pid int) (*os.Process, error) { return os.FindProcess(pid) }
