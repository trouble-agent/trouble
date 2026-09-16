package types

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

// ulidAlphabet is Crockford base32 (no I, L, O, U).
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// TsLayout is the pinned timestamp encoding: RFC3339 UTC, millisecond
// precision, always Z (SPEC-TYPES §6.1).
const TsLayout = "2006-01-02T15:04:05.000Z"

// NowUTC returns the current UTC time as an RFC3339 millisecond timestamp.
func NowUTC() string { return time.Now().UTC().Format(TsLayout) }

// FormatUTC renders t in the pinned layout.
func FormatUTC(t time.Time) string { return t.UTC().Format(TsLayout) }

// ParseUTC parses the pinned layout (or RFC3339Nano) back to a time.
func ParseUTC(s string) (time.Time, error) {
	if t, err := time.Parse(TsLayout, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

var (
	idMu     sync.Mutex
	idLastMS uint64
	idRand   [10]byte
)

// NewID mints "<prefix><ULID>" (SPEC-01 §3.2). The ULID is 48-bit millisecond
// timestamp + 80-bit randomness, Crockford base32, 26 characters, uppercase.
// Ids minted by one process are strictly increasing in string order.
func NewID(prefix Prefix) string {
	idMu.Lock()
	defer idMu.Unlock()
	ms := uint64(time.Now().UnixMilli())
	switch {
	case ms > idLastMS:
		idLastMS = ms
		if _, err := rand.Read(idRand[:]); err != nil {
			// crypto/rand failing is not recoverable for identity; fall back to
			// the timestamp alone (still monotonic, entropy degraded).
			idRand = [10]byte{}
			binary.BigEndian.PutUint64(idRand[2:], ms)
		}
	case !incRand(&idRand):
		idLastMS++
		idRand = [10]byte{}
	}
	return string(prefix) + encodeULID(idLastMS, idRand)
}

func incRand(r *[10]byte) bool {
	for i := len(r) - 1; i >= 0; i-- {
		r[i]++
		if r[i] != 0 {
			return true
		}
	}
	return false
}

// encodeULID packs 128 bits into 26 Crockford base32 characters (the standard
// 2-bit left padding). It is a straight bit walk: no big.Int, no allocation
// beyond the result, because NewID is on every record's hot path.
//
// Layout: 48-bit millisecond timestamp in the leading characters, 80-bit
// randomness in the trailing ones. That is what makes the JSONL-visible id order
// match time order, and it is why a same-millisecond caller must increment the
// randomness from its least significant byte (`incRand`) for ids to stay
// distinct and increasing.
func encodeULID(ms uint64, rnd [10]byte) string {
	var raw [16]byte
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	copy(raw[6:], rnd[:])
	var out [26]byte
	for i := 0; i < 26; i++ {
		// Character i covers the 130-bit encoding space bits [5i, 5i+5); the
		// value bits start two positions in (the 2-bit left padding).
		var v uint32
		for b := 0; b < 5; b++ {
			idx := 5*i + b - 2
			var bit uint32
			if idx >= 0 && idx < 128 {
				bit = uint32((raw[idx/8] >> uint(7-idx%8)) & 1)
			}
			v = v<<1 | bit
		}
		out[i] = ulidAlphabet[v]
	}
	return string(out[:])
}

// ParseID validates that s is "<prefix><26-char ULID>" and returns s.
func ParseID(prefix Prefix, s string) (string, error) {
	if !strings.HasPrefix(s, string(prefix)) {
		return "", ErrBadPrefix
	}
	body := s[len(prefix):]
	if len(body) != 26 {
		return "", ErrBadID
	}
	for i := 0; i < len(body); i++ {
		if !strings.ContainsRune(ulidAlphabet, rune(body[i])) {
			return "", ErrBadID
		}
	}
	return s, nil
}
