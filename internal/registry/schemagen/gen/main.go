// Command gen regenerates the committed module schemas. `make schema` runs it;
// `git diff --exit-code internal/registry/schema/` is part of CI, so a descriptor
// edit without a regenerated schema fails the build (SPEC-06 §3.2).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/totalwindupflightsystems/trouble/internal/registry"
)

func main() {
	dir := "internal/registry/schema"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := registry.WriteSchemas(dir); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
	names, _ := os.ReadDir(dir)
	fmt.Printf("wrote %d schema(s) to %s\n", len(names), filepath.Clean(dir))
}
