package main

import (
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func main() {
	seen := map[string]int{}
	for i := 0; i < 1000; i++ {
		seen[types.NewID(types.PInc)]++
	}
	dup, max := 0, 0
	for _, n := range seen {
		if n > 1 {
			dup++
		}
		if n > max {
			max = n
		}
	}
	fmt.Printf("1000 NewID calls -> %d distinct, %d duplicated values, max multiplicity %d\n", len(seen), dup, max)
	seen2 := map[string]int{}
	for i := 0; i < 1000; i++ {
		seen2[types.NewID(types.PEv)]++
	}
	dup2, max2 := 0, 0
	for _, n := range seen2 {
		if n > 1 {
			dup2++
		}
		if n > max2 {
			max2 = n
		}
	}
	fmt.Printf("1000 ev_ NewID calls -> %d distinct, %d duplicated values, max multiplicity %d\n", len(seen2), dup2, max2)
}
