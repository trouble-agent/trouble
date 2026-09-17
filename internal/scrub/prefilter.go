package scrub

import (
	"strings"
)

// signalMask names the trigger families that are not expressible as a literal
// keyword.
type signalMask uint8

const (
	sigEntropyRun   signalMask = 1 << iota // a run of ≥40 token characters
	sigEmailAt                             // an '@'
	sigIPv4                                // digit '.' digit
	sigPathSegments                        // an absolute path token
)

// trigWords is the engine-wide universe of prefilter keywords: every rule's
// anchors, deduplicated. A rule's gate is a bitmask over this universe.
type trigWords struct {
	words  []string
	bit    map[string]int
	prefix []uint64         // 65536-bit map: "these two bytes start some keyword"
	pairs  map[uint16][]int // lower-cased 2-byte prefix → word indexes
	nbits  int
}

func newTrigWords() *trigWords {
	t := &trigWords{bit: map[string]int{}, prefix: make([]uint64, 1024), pairs: map[uint16][]int{}}
	for i := range builtinTable {
		t.add(builtinTable[i].anchors)
	}
	return t
}

func (t *trigWords) add(words []string) {
	for _, w := range words {
		lw := strings.ToLower(w)
		if lw == "" {
			continue
		}
		if _, ok := t.bit[lw]; ok {
			continue
		}
		t.bit[lw] = t.nbits
		t.words = append(t.words, lw)
		t.nbits++
		if len(lw) >= 2 {
			k := uint16(lw[0])<<8 | uint16(lw[1])
			t.prefix[k>>6] |= 1 << (k & 63)
			t.pairs[k] = append(t.pairs[k], t.bit[lw])
		}
	}
}

// maskFor returns the bitmask of a rule's own keywords.
func (t *trigWords) maskFor(words []string) []uint64 {
	m := make([]uint64, t.words64())
	for _, w := range words {
		if i, ok := t.bit[strings.ToLower(w)]; ok {
			m[i>>6] |= 1 << uint(i&63)
		}
	}
	return m
}

func (t *trigWords) words64() int { return (t.nbits + 63) / 64 }

func maskEmpty(m []uint64) bool {
	for _, w := range m {
		if w != 0 {
			return false
		}
	}
	return true
}

func intersects(m []uint64, other []uint64) bool {
	for i := range m {
		if i < len(other) && m[i]&other[i] != 0 {
			return true
		}
	}
	return false
}

// fire scans b once and reports which trigger keywords and signals fired. It is
// the whole prefilter: one linear pass, no RE2 program, no allocation beyond
// the caller's mask slice.
func (t *trigWords) fire(b []byte, matched []uint64) signalMask {
	var sigs signalMask
	if len(b) == 0 {
		return 0
	}
	run := 0
	for i := 0; i < len(b); i++ {
		c := b[i]
		// signals
		if c == '@' {
			sigs |= sigEmailAt
		} else if c == '.' && i > 0 && i+1 < len(b) && isDigitByte(b[i-1]) && isDigitByte(b[i+1]) {
			sigs |= sigIPv4
		} else if c == '/' && (i == 0 || isPathContext(b[i-1])) && pathSegmentsFrom(b, i) >= 3 {
			sigs |= sigPathSegments
		}
		if isEntropyByte(c) {
			run++
			if run == 40 {
				sigs |= sigEntropyRun
			}
		} else {
			run = 0
		}
		// keywords: a 2-byte prefix map gates the expensive compare
		if i+1 < len(b) {
			k := pairKey(c, b[i+1])
			if t.prefix[k>>6]&(1<<(k&63)) != 0 {
				for _, idx := range t.pairs[k] {
					if hasFoldAt(b, i, t.words[idx]) {
						matched[idx>>6] |= 1 << uint(idx&63)
					}
				}
			}
		}
	}
	return sigs
}

// hasFoldAt reports whether b[i:] starts with w (ASCII case-insensitive).
func hasFoldAt(b []byte, i int, w string) bool {
	if i+len(w) > len(b) {
		return false
	}
	for j := 0; j < len(w); j++ {
		if lowerASCII(b[i+j]) != w[j] {
			return false
		}
	}
	return true
}

// lowerASCII maps an ASCII letter to lower case and leaves every other byte
// alone. It is length preserving.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func pairKey(c1, c2 byte) uint16 {
	return uint16(lowerASCII(c1))<<8 | uint16(lowerASCII(c2))
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isEntropyByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '-', c == '+', c == '/':
		return true
	}
	return false
}

// pathSegmentsFrom counts the path segments starting at an absolute path token
// at b[i].
func pathSegmentsFrom(b []byte, i int) int {
	segs, j := 0, i
	for j < len(b) && b[j] == '/' {
		k := j + 1
		for k < len(b) && isPathByte(b[k]) {
			k++
		}
		if k == j+1 {
			break
		}
		segs++
		j = k
	}
	return segs
}

func isPathByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-':
		return true
	}
	return false
}

func isPathContext(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', '(', '=', ':', ',':
		return true
	}
	return false
}
