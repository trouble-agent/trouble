package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/trouble-agent/trouble/internal/types"
)

// DefaultOffsetStride is the §2.5 `ledger.offset_stride` default: one recorded
// byte offset every N records, which bounds a page read to
// `offset_stride + page_size` lines (SPEC-01 §3.7a, §7).
const DefaultOffsetStride = 256

// sidecarSuffix is the §3.7a per-file footer index name.
const sidecarSuffix = ".idx"

// sidecarState records why a sidecar had to be rebuilt. The sidecar is derived
// state: its loss is a latency event, never a correctness event (§3.7a, §5), so
// no state here is an error — it is accounting.
type sidecarState string

const (
	sidecarOK         sidecarState = ""
	sidecarMissing    sidecarState = "missing"
	sidecarUnparsable sidecarState = "unparsable"
	sidecarMismatch   sidecarState = "mismatch"
)

// sidecarPath is the sidecar for a generation file (SPEC-01 §3.7a).
func sidecarPath(root, file string) string {
	return filepath.Join(root, file+sidecarSuffix)
}

// removeSidecar unlinks a generation file's sidecar. Retention drops whole
// generations (§3.7a), so the file and its `.idx` leave together; a sidecar left
// behind would describe a file that no longer exists.
func removeSidecar(root, file string) {
	if err := os.Remove(sidecarPath(root, file)); err != nil && !os.IsNotExist(err) {
		return
	}
}

// writeSidecar derives and writes `{file}.idx` for a generation file, fsyncing it
// before the file is announced as authoritative (§3.7a: written at rotation, and
// for the new generation at compaction). It returns the index it wrote so the
// caller can publish it without re-reading the file.
func writeSidecar(root, file string) (types.GenerationIndex, error) {
	gi, err := buildGenerationIndex(filepath.Join(root, file), file)
	if err != nil {
		return types.GenerationIndex{}, err
	}
	b, merr := json.Marshal(gi)
	if merr != nil {
		return gi, merr
	}
	tmp := sidecarPath(root, file) + ".tmp"
	f, oerr := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if oerr != nil {
		return gi, oerr
	}
	if _, werr := f.Write(append(b, '\n')); werr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return gi, werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return gi, serr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return gi, cerr
	}
	if rerr := os.Rename(tmp, sidecarPath(root, file)); rerr != nil {
		_ = os.Remove(tmp)
		return gi, rerr
	}
	return gi, syncDir(root)
}

// readSidecar reads and parses a sidecar. A missing or unparsable sidecar is a
// rebuild, never an error (SPEC-01 §3.7a, §5).
func readSidecar(path string) (types.GenerationIndex, sidecarState) {
	b, err := os.ReadFile(path)
	if err != nil {
		return types.GenerationIndex{}, sidecarMissing
	}
	var gi types.GenerationIndex
	if err := json.Unmarshal(bytes.TrimSpace(b), &gi); err != nil || gi.File == "" {
		return types.GenerationIndex{}, sidecarUnparsable
	}
	return gi, sidecarOK
}

// loadOrRebuildSidecar is the §3.7a accelerator path: an existing sidecar whose
// recorded Bytes/Sha256 agree with the file it names is used as-is; a missing,
// unparsable or mismatched sidecar is discarded, rebuilt from the generation
// file, and the reason returned so the caller can count it into IndexStats. "An
// `.idx` never gets to describe a file it does not match."
func loadOrRebuildSidecar(root, file string) (types.GenerationIndex, sidecarState, error) {
	gi, st := readSidecar(sidecarPath(root, file))
	if st == sidecarOK {
		ok, err := sidecarMatches(gi, filepath.Join(root, file))
		if err == nil && ok {
			return gi, sidecarOK, nil
		}
		if err != nil {
			// the file itself is unreadable: 011 is the honest existing code
			return types.GenerationIndex{}, st, ledgerErr(types.CodeLedger011, ReasonIO,
				fmt.Sprintf("cannot verify sidecar for %s", file), err)
		}
		st = sidecarMismatch
	}
	rebuilt, err := writeSidecar(root, file)
	if err != nil {
		return types.GenerationIndex{}, st, ledgerErr(types.CodeLedger011, ReasonIO,
			fmt.Sprintf("cannot rebuild sidecar for %s", file), err)
	}
	return rebuilt, st, nil
}

// sidecarMatches reports whether the sidecar's Bytes and Sha256 agree with the
// file it names. A sidecar that describes fewer bytes than the file holds is a
// mismatch too: the file changed under it (a part rollover, an interrupted
// rotation's crash-truncated sidecar).
func sidecarMatches(gi types.GenerationIndex, path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if gi.Bytes != fi.Size() || gi.Sha256 == "" {
		return false, nil
	}
	sum, err := sha256File(path)
	if err != nil {
		return false, err
	}
	return sum == gi.Sha256, nil
}

// sha256File streams a file's bytes through sha256 (never buffering it whole).
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildGenerationIndex derives a §3.7a sidecar from the generation file itself:
// record count, byte offsets every OffsetStride records, seq range, min/max ts,
// torn lines, and the sha256 the archive marker id derives from.
//
// It streams the file once and holds one line at a time (the same discipline the
// index rebuild uses), so the sidecar of a large generation costs a scan and not
// a buffer.
func buildGenerationIndex(path, file string) (types.GenerationIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return types.GenerationIndex{}, err
	}
	defer f.Close()
	gi := types.GenerationIndex{File: file, OffsetStride: DefaultOffsetStride, Offsets: []int64{}}
	h := sha256.New()
	if fi, serr := f.Stat(); serr == nil {
		gi.Bytes = fi.Size()
	}
	rd := newLineReader(f)
	var lineNo int64
	for {
		line, start, complete, rerr := rd.next()
		if rerr != nil {
			return types.GenerationIndex{}, rerr
		}
		if len(line) == 0 && !complete {
			break
		}
		// hash the bytes exactly as the file holds them: line + its terminator
		h.Write(line)
		if complete {
			h.Write([]byte{'\n'})
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, derr := decodeRecord(line, false)
		if derr != nil || rec == nil {
			if !complete {
				gi.TornLines++
			}
			continue
		}
		lineNo++
		gi.Records++
		if gi.FirstSeq == 0 || rec.Seq < gi.FirstSeq {
			gi.FirstSeq = rec.Seq
		}
		if rec.Seq > gi.LastSeq {
			gi.LastSeq = rec.Seq
		}
		if gi.MinTS == "" || rec.TS < gi.MinTS {
			gi.MinTS = rec.TS
		}
		if rec.TS > gi.MaxTS {
			gi.MaxTS = rec.TS
		}
		if (lineNo-1)%int64(gi.OffsetStride) == 0 {
			gi.Offsets = append(gi.Offsets, start)
		}
	}
	// A torn tail is not a record: a file of complete lines hashes to exactly its
	// size, and that is the invariant sidecarMatches re-checks.
	gi.Sha256 = hex.EncodeToString(h.Sum(nil))
	return gi, nil
}

// cachedSidecar returns a file's §3.7a sidecar, rebuilding it when the cache is
// cold, the sidecar is missing/unparsable, or it no longer matches the file
// (the file changed under the process). size is the caller's view of the file
// size, which doubles as the cache's invalidation key for the live file.
//
// The rebuild is counted, never raised: the sidecar is an accelerator and its
// loss is a latency event, not a correctness event (§3.7a, §5).
func (l *Ledger) cachedSidecar(file string, size int64) (types.GenerationIndex, bool) {
	l.sidecarMu.Lock()
	if e, ok := l.sidecarCache[file]; ok && e.size == size && e.gi.File != "" {
		l.sidecarMu.Unlock()
		return e.gi, true
	}
	l.sidecarMu.Unlock()

	gi, st, err := loadOrRebuildSidecar(l.root, file)
	if err != nil {
		l.sidecarFailures.Add(1)
		return types.GenerationIndex{}, false
	}
	if st != sidecarOK {
		l.sidecarRebuildsCount.Add(1)
	}
	l.sidecarMu.Lock()
	l.sidecarCache[file] = sidecarEntry{gi: gi, size: gi.Bytes}
	l.sidecarMu.Unlock()
	return gi, true
}

// writeGenerationSidecar writes the sidecar for a file the writer has just
// closed, counting a failure instead of failing the data path: the sidecar is an
// accelerator and the generation file is the durable artifact (§3.7a).
func (l *Ledger) writeGenerationSidecar(file string) {
	gi, err := writeSidecar(l.root, file)
	if err != nil {
		l.sidecarFailures.Add(1)
		return
	}
	l.sidecarMu.Lock()
	l.sidecarCache[file] = sidecarEntry{gi: gi, size: gi.Bytes}
	l.sidecarMu.Unlock()
}

// SidecarRebuilds reports how many sidecars this process had to rebuild — the
// accounting §3.7a asks for, since a rebuilt sidecar is a latency event.
func (l *Ledger) SidecarRebuilds() uint64 { return l.sidecarRebuildsCount.Load() }

// SidecarFailures reports how many sidecar writes/rebuilds failed outright.
func (l *Ledger) SidecarFailures() uint64 { return l.sidecarFailures.Load() }

// GenerationSidecar exposes a generation file's `.idx` (rebuilding it when
// necessary) so the CLI and the hub read the same side the writer maintains.
func (l *Ledger) GenerationSidecar(file string) (types.GenerationIndex, bool) {
	fi, err := os.Stat(filepath.Join(l.root, file))
	if err != nil {
		return types.GenerationIndex{}, false
	}
	return l.cachedSidecar(file, fi.Size())
}
