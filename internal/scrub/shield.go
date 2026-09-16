package scrub

import (
	"bytes"
	"strconv"
)

// shield protects the DSN public keys of the configured projects from the rule
// pass (SPEC-02 §3.5).
//
// Mechanism: before rule 1, each known public key occurrence is replaced by the
// placeholder "\x00S<index>\x00"; after rule 18 the placeholders are restored.
// NUL is valid UTF-8 and appears in no rule's alphabet, so no rule can match a
// placeholder, and the restored value is the designed-public credential.
//
// A placeholder is also recognised by the DSN parsers (rules 3/4): the DSN of a
// project may legitimately appear inside that project's own exception message
// (§3.5 case 1), and the secret half must still be redacted even though the
// public half is shielded. The parser therefore treats a placeholder in the
// public-key slot as the public key it stands for, and the restore step puts
// the key back verbatim.
type shield struct {
	keys []string
	ph   [][]byte
}

func newShield(keys []string) *shield {
	if len(keys) == 0 {
		return &shield{}
	}
	s := &shield{keys: make([]string, 0, len(keys)), ph: make([][]byte, 0, len(keys))}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		s.keys = append(s.keys, k)
		s.ph = append(s.ph, placeholderFor(len(s.ph)))
	}
	return s
}

func placeholderFor(i int) []byte {
	b := make([]byte, 0, 6)
	b = append(b, 0x00, 'S')
	b = strconv.AppendInt(b, int64(i), 10)
	b = append(b, 0x00)
	return b
}

func (s *shield) empty() bool { return s == nil || len(s.keys) == 0 }

// protect replaces every known public key occurrence with its placeholder. The
// input is returned unchanged (same slice) when no key occurs.
func (s *shield) protect(b []byte) []byte {
	if s.empty() || len(b) == 0 {
		return b
	}
	out := b
	for i, k := range s.keys {
		if !bytes.Contains(out, []byte(k)) {
			continue
		}
		out = bytes.ReplaceAll(out, []byte(k), s.ph[i])
	}
	return out
}

// restore puts every public key back verbatim.
func (s *shield) restore(b []byte) []byte {
	if s.empty() || len(b) == 0 {
		return b
	}
	out := b
	for i, k := range s.keys {
		if !bytes.Contains(out, s.ph[i]) {
			continue
		}
		out = bytes.ReplaceAll(out, s.ph[i], []byte(k))
	}
	return out
}

// indexOfPlaceholder reports whether tok is one of this shield's placeholder
// byte strings and returns its index.
func (s *shield) indexOfPlaceholder(tok []byte) (int, bool) {
	if s.empty() {
		return 0, false
	}
	for i, p := range s.ph {
		if bytes.Equal(tok, p) {
			return i, true
		}
	}
	return 0, false
}

// containsPlaceholder reports whether b contains any placeholder byte string.
func (s *shield) containsPlaceholder(b []byte) bool {
	if s.empty() {
		return false
	}
	for _, p := range s.ph {
		if bytes.Contains(b, p) {
			return true
		}
	}
	return false
}
