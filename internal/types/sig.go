package types

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SigAlgo is the digest algorithm of a sig. sha256 is the only value in v0.1.
const SigAlgoSHA256 = "sha256"

// NormVersionV1 is the normalization version pinned by SPEC-01 §3.3.
const NormVersionV1 = 1

// SigSource enumerates the dedup-core sources (SPEC-TYPES §3.4).
type SigSource string

const (
	SrcSentinel  SigSource = "sentinel"
	SrcJournald  SigSource = "journald"
	SrcPSI       SigSource = "psi"
	SrcDBus      SigSource = "dbus"
	SrcDisk      SigSource = "disk"
	SrcTimers    SigSource = "timers"
	SrcInotify   SigSource = "inotify"
	SrcCollector SigSource = "collector"
	SrcGeneric   SigSource = "generic"
	SrcUnknown   SigSource = "unknown"
)

// SigSources lists every valid source.
var SigSources = []SigSource{
	SrcSentinel, SrcJournald, SrcPSI, SrcDBus, SrcDisk,
	SrcTimers, SrcInotify, SrcCollector, SrcGeneric, SrcUnknown,
}

// Valid reports whether s is a known sig source.
func (s SigSource) Valid() bool {
	for _, v := range SigSources {
		if v == s {
			return true
		}
	}
	return false
}

// Sig is the dedup-core signature (SPEC-TYPES §3.4, §6.3).
//
// The canonical string is source + ":" + algo + "v" + itoa(norm_version) + ":" +
// hex(digest)[:16]; grouping compares Digest (the full 32 bytes).
type Sig struct {
	Source      SigSource `json:"source"`
	Algo        string    `json:"algo"`
	NormVersion int       `json:"norm_version"`
	Digest      []byte    `json:"digest"`
	Short       string    `json:"short"`
}

// NewSig builds a Sig from a full digest.
func NewSig(source SigSource, algo string, normVersion int, digest []byte) Sig {
	d := make([]byte, len(digest))
	copy(d, digest)
	s := Sig{Source: source, Algo: algo, NormVersion: normVersion, Digest: d}
	if len(d) >= 8 {
		s.Short = hex.EncodeToString(d)[:16]
	}
	return s
}

// String returns the canonical sig string.
func (s Sig) String() string {
	return string(s.Source) + ":" + s.Algo + "v" + strconv.Itoa(s.NormVersion) + ":" + s.Short
}

// DigestHex is the full 32-byte digest as 64 hex characters (the dedup truth).
func (s Sig) DigestHex() string { return hex.EncodeToString(s.Digest) }

// ParseSig parses a canonical sig string.
func ParseSig(s string) (Sig, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return Sig{}, fmt.Errorf("types: sig %q is not <source>:<algo>v<n>:<hex16>", s)
	}
	src := SigSource(parts[0])
	if !src.Valid() {
		return Sig{}, fmt.Errorf("types: sig %q has unknown source", s)
	}
	algoVer := parts[1]
	i := strings.Index(algoVer, "v")
	if i <= 0 {
		return Sig{}, fmt.Errorf("types: sig %q has no norm version", s)
	}
	algo := algoVer[:i]
	n, err := strconv.Atoi(algoVer[i+1:])
	if err != nil || n < 1 {
		return Sig{}, fmt.Errorf("types: sig %q has an invalid norm version", s)
	}
	short := parts[2]
	if len(short) != 16 {
		return Sig{}, fmt.Errorf("types: sig %q short form must be 16 hex chars", s)
	}
	if _, err := hex.DecodeString(short); err != nil {
		return Sig{}, fmt.Errorf("types: sig %q short form is not hex", s)
	}
	return Sig{Source: src, Algo: algo, NormVersion: n, Short: short}, nil
}

// SigDigest returns sha256(b).
func SigDigest(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// NormalizedBytes frames normalized field values for a source (SPEC-01 §3.3):
// source + "\x1f" + field₁ + "\x1f" + field₂ … + "\x1e", with \x00/\x1f/\x1e
// inside a value replaced by one space and values trimmed.
func NormalizedBytes(source SigSource, fields ...string) []byte {
	var b strings.Builder
	b.WriteString(string(source))
	for _, f := range fields {
		b.WriteByte(0x1f)
		b.WriteString(cleanField(f))
	}
	b.WriteByte(0x1e)
	return []byte(b.String())
}

// NormalizedFromBytes frames already-jointed fields (see NormalizedBytes).
func NormalizedFromBytes(source SigSource, joined string) []byte {
	return []byte(string(source) + "\x1f" + joined + "\x1e")
}

func cleanField(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case 0x00, 0x1f, 0x1e:
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// NewSigFromFields computes the v1 signature of a source's normalized fields.
func NewSigFromFields(source SigSource, fields ...string) Sig {
	return NewSig(source, SigAlgoSHA256, NormVersionV1, SigDigest(NormalizedBytes(source, fields...)))
}

// DigestShort is hex(digest)[:16], the display + ledger sig short form.
func DigestShort(digest []byte) string {
	h := hex.EncodeToString(digest)
	if len(h) < 16 {
		return h
	}
	return h[:16]
}

// Errors returned by the identity helpers.
var (
	ErrBadID     = errors.New("types: malformed id")
	ErrBadSig    = errors.New("types: malformed sig")
	ErrBadPrefix = errors.New("types: wrong id prefix")
)
