package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// NewTarget resolves the archive target from configuration (SPEC-13 §3.5 step 2:
// "write one object per generation into the DuckBrain namespace").
//
// The refusals are TROUBLE-HUB-009, class permanent, and they are the reason the
// archival tier "does not start" instead of writing somewhere else: an
// unconfigured namespace or a missing endpoint is a configuration error, and the
// queue keeps every pending generation until an operator fixes it (SPEC-13 §5).
func NewTarget(cfg ArchiveConfig) (Target, error) {
	cfg = cfg.WithDefaults()
	if strings.TrimSpace(cfg.Namespace) == "" {
		return nil, errf(types.CodeHub009, ReasonArchiveCfg,
			"server.duckbrain.namespace is empty: the archival target is unusable (SPEC-13 §3.5 step 2)")
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errf(types.CodeHub009, ReasonArchiveCfg,
			"server.duckbrain.endpoint is empty: no DuckBrain driver endpoint to write to")
	}
	return NewKVTarget(cfg)
}

// KVTarget writes generations into a DuckBrain namespace over the KV contract
// internal/issues' duckbrain driver already speaks in this tree: `PUT/GET
// /v1/kv/<key>` with a `{"value": …}` JSON body and a configured credential
// header. The contract is reused rather than invented, so the archive tier is
// the same backend the issue desk writes to.
//
// The KV value is JSON and a generation is bytes, so the bytes ride base64 with
// their length and sha256 beside them: the read-back verification of §3.5 step 3
// compares the DECODED bytes against the marker, and the envelope's own sha256
// gives a second, independent check that the transport did not mangle it.
type KVTarget struct {
	Base   string
	Header string
	Scheme string
	Key    string

	hc *http.Client

	Puts   atomic.Int64
	Gets   atomic.Int64
	Errors atomic.Int64
}

// NewKVTarget builds the HTTP target from configuration.
func NewKVTarget(cfg ArchiveConfig) (*KVTarget, error) {
	cfg = cfg.WithDefaults()
	base := strings.TrimRight(cfg.Endpoint, "/")
	if base == "" {
		return nil, errf(types.CodeHub009, ReasonArchiveCfg, "the duckbrain endpoint is empty")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	key, err := resolveArchiveKey(cfg)
	if err != nil {
		return nil, err
	}
	return &KVTarget{
		Base:   base,
		Header: cfg.KeyHeader,
		Key:    key,
		hc:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// resolveArchiveKey reads the credential from the environment first and the
// declared 0600 file second. The value never reaches a log, a record or an error
// message (SPEC-12 §3.2's rule, applied to the archival tier).
func resolveArchiveKey(cfg ArchiveConfig) (string, error) {
	if v := strings.TrimSpace(os.Getenv(cfg.KeyEnv)); v != "" {
		return v, nil
	}
	if cfg.KeyFile == "" {
		return "", nil
	}
	path := cfg.KeyFile
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", nil // an unset key file is not an error for a local-first backend
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", errf(types.CodeHub009, ReasonArchiveCfg,
			"duckbrain key file mode %04o is group/world accessible: refusing to read it", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot read the duckbrain key file", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Describe names the target without any part of the credential.
func (t *KVTarget) Describe() string {
	return fmt.Sprintf("duckbrain kv %s (header %s, credential %s)", t.Base, t.Header, presentOrAbsent(t.Key))
}

func presentOrAbsent(v string) string {
	if v == "" {
		return "absent"
	}
	return "present"
}

// kvValue is the object body (`{"value": …}` per the driver contract).
type kvValue struct {
	Value kvBlob `json:"value"`
}

// kvBlob is the JSON form of binary object bytes.
type kvBlob struct {
	Encoding string `json:"encoding"`
	Bytes    int64  `json:"bytes"`
	Sha256   string `json:"sha256"`
	Data     string `json:"data"`
}

// Put writes one object (step 2).
func (t *KVTarget) Put(ctx context.Context, key string, data []byte) error {
	body := kvValue{Value: kvBlob{
		Encoding: "base64",
		Bytes:    int64(len(data)),
		Sha256:   Sha256Hex(data),
		Data:     base64.StdEncoding.EncodeToString(data),
	}}
	raw, err := json.Marshal(body)
	if err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot encode the object", err)
	}
	status, err := t.do(ctx, http.MethodPut, key, raw)
	t.Puts.Add(1)
	if err != nil {
		t.Errors.Add(1)
		return tenantErr(err, status, "put "+key)
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNoContent {
		t.Errors.Add(1)
		return tenantErr(nil, status, "put "+key)
	}
	return nil
}

// Get reads one object back (step 3).
func (t *KVTarget) Get(ctx context.Context, key string) ([]byte, error) {
	status, body, err := t.doRaw(ctx, http.MethodGet, key, nil)
	t.Gets.Add(1)
	if err != nil {
		t.Errors.Add(1)
		return nil, tenantErr(err, status, "get "+key)
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		t.Errors.Add(1)
		return nil, errf(types.CodeHub012, ReasonVerify, "object %s is not present at the target", key)
	}
	if status != http.StatusOK {
		t.Errors.Add(1)
		return nil, tenantErr(nil, status, "get "+key)
	}
	var envelope kvValue
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Errors.Add(1)
		return nil, errWrap(types.CodeHub012, ReasonVerify, "object "+key+" is not a kv value", err)
	}
	raw, derr := base64.StdEncoding.DecodeString(envelope.Value.Data)
	if derr != nil {
		t.Errors.Add(1)
		return nil, errWrap(types.CodeHub012, ReasonVerify, "object "+key+" holds an undecodable value", derr)
	}
	if envelope.Value.Bytes != 0 && int64(len(raw)) != envelope.Value.Bytes {
		t.Errors.Add(1)
		return nil, errf(types.CodeHub012, ReasonVerify,
			"object %s: transport length %d does not match its envelope %d", key, len(raw), envelope.Value.Bytes)
	}
	if envelope.Value.Sha256 != "" && Sha256Hex(raw) != envelope.Value.Sha256 {
		t.Errors.Add(1)
		return nil, errf(types.CodeHub012, ReasonVerify,
			"object %s: transport sha256 does not match its envelope", key)
	}
	return raw, nil
}

func (t *KVTarget) do(ctx context.Context, method, key string, payload []byte) (int, error) {
	status, _, err := t.doRaw(ctx, method, key, payload)
	return status, err
}

func (t *KVTarget) doRaw(ctx context.Context, method, key string, payload []byte) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.Base+"/v1/kv/"+url.PathEscape(key), body)
	if err != nil {
		return 0, nil, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot build the request", err)
	}
	req.Header.Set("Accept", "application/json")
	if t.Key != "" {
		v := t.Key
		if t.Scheme != "" {
			v = t.Scheme + " " + t.Key
		}
		req.Header.Set(t.Header, v)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, out, nil
}

// tenantErr classifies a target failure into the two codes the spec names.
func tenantErr(err error, status int, what string) *Error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errf(types.CodeHub009, ReasonArchiveCfg, "%s: the archival credential was refused (status %d)", what, status)
	case status == http.StatusBadRequest || status == http.StatusNotFound && strings.HasPrefix(what, "put"):
		return errf(types.CodeHub009, ReasonArchiveCfg, "%s: the namespace is not writable (status %d)", what, status)
	case err != nil:
		return errWrap(types.CodeHub010, ReasonDuckBrain, what+": the archival tier is unreachable", err)
	default:
		return errf(types.CodeHub010, ReasonDuckBrain, "%s: the archival tier answered %d", what, status)
	}
}

// DirTarget is a filesystem object store under one directory. It is the target
// this package's tests use (and the shape a local disk tier takes): the same
// Put/Get contract, no network, and the object key's path separators become
// directory separators.
type DirTarget struct {
	Root string
}

// NewDirTarget builds a filesystem target rooted at dir.
func NewDirTarget(dir string) *DirTarget { return &DirTarget{Root: dir} }

// Describe names the target.
func (d *DirTarget) Describe() string { return "directory " + d.Root }

// Put writes an object (creating parent directories).
func (d *DirTarget) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(d.Root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot create the object directory", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// Get reads an object back.
func (d *DirTarget) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(d.Root, filepath.FromSlash(key)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errf(types.CodeHub012, ReasonVerify, "object %s is not present at the target", key)
		}
		return nil, errWrap(types.CodeHub010, ReasonDuckBrain, "cannot read object "+key, err)
	}
	return b, nil
}
