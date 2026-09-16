package types

import (
	"crypto/rand"
	"testing"
)

func TestDbgULID(t *testing.T) {
	b := make([]byte, 16)
	n, err := rand.Read(b)
	t.Logf("rand.Read n=%d err=%v bytes=%x", n, err, b)
	a, c := NewID(PEv), NewID(PEv)
	t.Logf("a=%s c=%s same=%v", a, c, a == c)
}
