//go:build !linux

package lifecycle

// findBindHolder on non-Linux builds: the /proc listener-table walk is
// Linux-only (TRBL-042), so elsewhere the bind refusal keeps its original
// bare diagnostic and the operator falls back to the ss/lsof hint.
func findBindHolder(host, port string) (bindHolder, bool) {
	return bindHolder{}, false
}
