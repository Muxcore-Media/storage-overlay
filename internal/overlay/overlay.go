package overlay

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Muxcore-Media/core/pkg/contracts"
)

var demoAESKey = bytesRepeat(0x11, 32)

// DemoAESKey returns the built-in demo key (testing only).
func DemoAESKey() []byte { return append([]byte(nil), demoAESKey...) }

// Config controls overlay transforms applied before the backend store.
type Config struct { //nolint:govet // fieldalignment: overlay flags grouped for readability
	Encrypt       bool
	Compress      bool
	Dedup         bool
	AESKey        []byte // 16/24/32 bytes when Encrypt
	BackendPrefix string
}

// ValidateEncryptKey rejects missing/invalid/demo keys when encryption is on.
func ValidateEncryptKey(encrypt bool, key []byte) error {
	if !encrypt {
		return nil
	}
	n := len(key)
	if n != 16 && n != 24 && n != 32 {
		return fmt.Errorf("AES key must be 16, 24, or 32 bytes (got %d)", n)
	}
	if bytes.Equal(key, demoAESKey) {
		return fmt.Errorf("refusing built-in demo AES key when encryption is enabled; set OVERLAY_AES_KEY_HEX")
	}
	return nil
}

// MemBackend is an in-memory StorageProvider used as the default backend.
type MemBackend struct { //nolint:govet // fieldalignment: mutex before map for readability
	mu   sync.RWMutex
	objs map[string]memObj
}

type memObj struct { //nolint:govet // fieldalignment: data before metadata timestamp
	data    []byte
	modTime time.Time
}

func NewMemBackend() *MemBackend {
	return &MemBackend{objs: map[string]memObj{}}
}

func (m *MemBackend) Put(_ context.Context, key string, data io.Reader, size int64) error {
	buf, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObj{data: append([]byte(nil), buf...), modTime: time.Now().UTC()}
	return nil
}

func (m *MemBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, contracts.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), o.data...))), nil
}

func (m *MemBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *MemBackend) Move(ctx context.Context, src, dst string) error {
	rc, err := m.Get(ctx, src)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if err := m.Put(ctx, dst, rc, -1); err != nil {
		return err
	}
	return m.Delete(ctx, src)
}

func (m *MemBackend) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.objs[key]
	return ok, nil
}

func (m *MemBackend) Stat(_ context.Context, key string) (contracts.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objs[key]
	if !ok {
		return contracts.ObjectInfo{}, contracts.ErrNotFound
	}
	return contracts.ObjectInfo{Key: key, Size: int64(len(o.data)), LastModified: o.modTime}, nil
}

func (m *MemBackend) List(_ context.Context, prefix string) ([]contracts.ObjectInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]contracts.ObjectInfo, 0)
	for k, o := range m.objs {
		if prefix != "" && !hasPrefix(k, prefix) {
			continue
		}
		out = append(out, contracts.ObjectInfo{Key: k, Size: int64(len(o.data)), LastModified: o.modTime})
	}
	return out, nil
}

// Health probes the in-memory store.
func (m *MemBackend) Health(context.Context) error { return nil }

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// BackendHealth is implemented by backends that support Health probes.
type BackendHealth interface {
	Health(ctx context.Context) error
}

// Provider wraps a backend StorageProvider with optional encrypt/compress/dedup.
type Provider struct { //nolint:govet // fieldalignment: mutex and config grouped for readability
	mu      sync.RWMutex
	cfg     Config
	backend contracts.StorageProvider
	// logical key -> content-addressed blob key (when Dedup)
	index map[string]string
	// content hash -> refcount
	refs map[string]int
	// logical key -> cached metadata
	meta map[string]objectMeta
}

// New builds an overlay Provider. backend may be nil (uses MemBackend).
func New(cfg Config, backend contracts.StorageProvider) (*Provider, error) {
	if err := ValidateEncryptKey(cfg.Encrypt, cfg.AESKey); err != nil {
		return nil, err
	}
	if backend == nil {
		backend = NewMemBackend()
	}
	return &Provider{
		cfg:     cfg,
		backend: backend,
		index:   map[string]string{},
		refs:    map[string]int{},
		meta:    map[string]objectMeta{},
	}, nil
}

// Init rebuilds in-memory dedup index/refcounts and metadata cache from the backend.
func (p *Provider) Init(ctx context.Context) error {
	cfg := p.Config()
	if cfg.Dedup {
		return p.rebuildDedupIndex(ctx)
	}
	return p.rebuildSidecarIndex(ctx)
}

func (p *Provider) rebuildDedupIndex(ctx context.Context) error {
	items, err := p.backend.List(ctx, p.backendKey("meta/"))
	if err != nil {
		return err
	}
	metaPrefix := p.backendKey("meta/")
	index := map[string]string{}
	refs := map[string]int{}
	meta := map[string]objectMeta{}
	for _, it := range items {
		logical := it.Key
		logical = strings.TrimPrefix(logical, metaPrefix)
		rc, err := p.backend.Get(ctx, p.backendKey("meta/"+logical))
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		om, err := parseMeta(raw)
		if err != nil || om.Hash == "" {
			continue
		}
		index[logical] = om.Hash
		refs[om.Hash]++
		meta[logical] = om
	}
	p.mu.Lock()
	p.index = index
	p.refs = refs
	p.meta = meta
	p.mu.Unlock()
	return nil
}

func (p *Provider) rebuildSidecarIndex(ctx context.Context) error {
	items, err := p.backend.List(ctx, p.backendKey("sidecar/"))
	if err != nil {
		return nil //nolint:nilerr // sidecars are optional; a failed listing just means none to index
	}
	meta := map[string]objectMeta{}
	prefix := p.backendKey("sidecar/")
	for _, it := range items {
		logical := strings.TrimPrefix(it.Key, prefix)
		if !strings.HasSuffix(logical, ".json") {
			continue
		}
		logical = strings.TrimSuffix(logical, ".json")
		rc, err := p.backend.Get(ctx, it.Key)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			continue
		}
		om, err := parseMeta(raw)
		if err != nil {
			continue
		}
		meta[logical] = om
	}
	p.mu.Lock()
	p.meta = meta
	p.mu.Unlock()
	return nil
}

func (p *Provider) Config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *Provider) ReplaceConfig(cfg Config) error {
	if err := ValidateEncryptKey(cfg.Encrypt, cfg.AESKey); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
	return nil
}

func (p *Provider) Backend() contracts.StorageProvider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.backend
}

func (p *Provider) Health(ctx context.Context) error {
	p.mu.RLock()
	b := p.backend
	p.mu.RUnlock()
	if b == nil {
		return fmt.Errorf("backend not configured")
	}
	if hc, ok := b.(BackendHealth); ok {
		return hc.Health(ctx)
	}
	_, err := b.Exists(ctx, p.backendKey(".overlay-health"))
	return err
}

func (p *Provider) backendKey(logical string) string {
	prefix := p.cfg.BackendPrefix
	if prefix == "" {
		return logical
	}
	return prefix + "/" + logical
}

func (p *Provider) Put(ctx context.Context, key string, data io.Reader, size int64) error {
	cfg := p.Config()
	mod := time.Now().UTC()

	tmp, err := os.CreateTemp("", "storage-overlay-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	hashSum, plainSize, err := streamTransformOut(tmp, cfg, data)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	_ = tmp.Close()

	readBlob, err := os.Open(tmpPath) //nolint:gosec // tmpPath is our own os.CreateTemp file, not user input
	if err != nil {
		return err
	}
	defer func() { _ = readBlob.Close() }()
	stat, err := readBlob.Stat()
	if err != nil {
		return err
	}
	blobSize := stat.Size()
	hashHex := hex.EncodeToString(hashSum)
	om := objectMeta{Size: size, LastModified: mod}
	if size <= 0 {
		om.Size = plainSize
	}

	if cfg.Dedup {
		blobKey := p.backendKey("blobs/" + hashHex)
		p.mu.Lock()
		prev, had := p.index[key]
		needBlob := false
		if !had || prev != hashHex {
			ok, existsErr := p.backend.Exists(ctx, blobKey)
			if existsErr != nil {
				p.mu.Unlock()
				_ = tmp.Close()
				return existsErr
			}
			needBlob = !ok
		}
		p.mu.Unlock()

		if needBlob {
			if putErr := p.backend.Put(ctx, blobKey, readBlob, blobSize); putErr != nil {
				return putErr
			}
		}

		p.mu.Lock()
		if !had || prev != hashHex {
			p.refs[hashHex]++
			if had {
				p.refs[prev]--
				if p.refs[prev] <= 0 {
					delete(p.refs, prev)
					prevKey := p.backendKey("blobs/" + prev)
					p.mu.Unlock()
					if delErr := p.backend.Delete(ctx, prevKey); delErr != nil {
						return delErr
					}
					p.mu.Lock()
				}
			}
			p.index[key] = hashHex
		}
		om.Hash = hashHex
		p.meta[key] = om
		p.mu.Unlock()

		metaBytes, encErr := encodeMeta(om)
		if encErr != nil {
			return encErr
		}
		return p.backend.Put(ctx, p.backendKey("meta/"+key), bytes.NewReader(metaBytes), int64(len(metaBytes)))
	}

	p.mu.Lock()
	p.meta[key] = om
	p.mu.Unlock()

	if putErr := p.backend.Put(ctx, p.backendKey(key), readBlob, blobSize); putErr != nil {
		return putErr
	}

	sidecar, err := encodeMeta(om)
	if err != nil {
		return err
	}
	return p.backend.Put(ctx, p.backendKey("sidecar/"+key+".json"), bytes.NewReader(sidecar), int64(len(sidecar)))
}

func (p *Provider) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	blobKey, err := p.resolveBlobKey(ctx, key)
	if err != nil {
		return nil, err
	}
	rc, err := p.backend.Get(ctx, blobKey)
	if err != nil {
		return nil, err
	}
	return p.decodeBlob(rc, p.Config())
}

func (p *Provider) decodeBlob(rc io.ReadCloser, cfg Config) (io.ReadCloser, error) {
	out, err := streamTransformIn(rc, cfg)
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	return &parentCloser{ReadCloser: out, parent: rc}, nil
}

type parentCloser struct {
	io.ReadCloser
	parent io.Closer
	closed bool
}

func (p *parentCloser) Close() error {
	if !p.closed {
		p.closed = true
		_ = p.ReadCloser.Close()
		return p.parent.Close()
	}
	return nil
}

func (p *Provider) resolveBlobKey(ctx context.Context, key string) (string, error) {
	cfg := p.Config()
	if !cfg.Dedup {
		return p.backendKey(key), nil
	}
	p.mu.RLock()
	hash, ok := p.index[key]
	p.mu.RUnlock()
	if ok {
		return p.backendKey("blobs/" + hash), nil
	}
	rc, err := p.backend.Get(ctx, p.backendKey("meta/"+key))
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return "", err
	}
	om, err := parseMeta(raw)
	if err != nil || om.Hash == "" {
		return "", fmt.Errorf("invalid meta for %q", key)
	}
	p.mu.Lock()
	p.index[key] = om.Hash
	p.refs[om.Hash]++
	p.meta[key] = om
	p.mu.Unlock()
	return p.backendKey("blobs/" + om.Hash), nil
}

func (p *Provider) Delete(ctx context.Context, key string) error {
	cfg := p.Config()
	if !cfg.Dedup {
		if err := p.backend.Delete(ctx, p.backendKey(key)); err != nil {
			return err
		}
		_ = p.backend.Delete(ctx, p.backendKey("sidecar/"+key+".json"))
		p.mu.Lock()
		delete(p.meta, key)
		p.mu.Unlock()
		return nil
	}

	var blobKey string
	p.mu.Lock()
	hash, ok := p.index[key]
	if ok {
		delete(p.index, key)
		delete(p.meta, key)
		p.refs[hash]--
		if p.refs[hash] <= 0 {
			delete(p.refs, hash)
			blobKey = p.backendKey("blobs/" + hash)
		}
	}
	p.mu.Unlock()

	if blobKey != "" {
		if err := p.backend.Delete(ctx, blobKey); err != nil {
			return err
		}
	}
	return p.backend.Delete(ctx, p.backendKey("meta/"+key))
}

func (p *Provider) Move(ctx context.Context, src, dst string) error {
	rc, err := p.Get(ctx, src)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	info, err := p.Stat(ctx, src)
	if err != nil {
		return err
	}
	if err := p.Put(ctx, dst, rc, info.Size); err != nil {
		return err
	}
	return p.Delete(ctx, src)
}

func (p *Provider) Exists(ctx context.Context, key string) (bool, error) {
	cfg := p.Config()
	p.mu.RLock()
	_, ok := p.index[key]
	p.mu.RUnlock()
	if cfg.Dedup {
		if ok {
			return true, nil
		}
		return p.backend.Exists(ctx, p.backendKey("meta/"+key))
	}
	return p.backend.Exists(ctx, p.backendKey(key))
}

func (p *Provider) Stat(ctx context.Context, key string) (contracts.ObjectInfo, error) {
	cfg := p.Config()
	if cfg.Dedup {
		p.mu.RLock()
		if om, ok := p.meta[key]; ok {
			p.mu.RUnlock()
			return contracts.ObjectInfo{
				Key: key, Size: om.Size, ContentType: om.ContentType, LastModified: om.LastModified,
			}, nil
		}
		p.mu.RUnlock()
		rc, err := p.backend.Get(ctx, p.backendKey("meta/"+key))
		if err != nil {
			return contracts.ObjectInfo{}, err
		}
		raw, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return contracts.ObjectInfo{}, err
		}
		om, err := parseMeta(raw)
		if err != nil {
			return contracts.ObjectInfo{}, err
		}
		p.mu.Lock()
		p.meta[key] = om
		p.mu.Unlock()
		return contracts.ObjectInfo{
			Key: key, Size: om.Size, ContentType: om.ContentType, LastModified: om.LastModified,
		}, nil
	}

	p.mu.RLock()
	if om, ok := p.meta[key]; ok {
		p.mu.RUnlock()
		return contracts.ObjectInfo{
			Key: key, Size: om.Size, ContentType: om.ContentType, LastModified: om.LastModified,
		}, nil
	}
	p.mu.RUnlock()

	rc, err := p.backend.Get(ctx, p.backendKey("sidecar/"+key+".json"))
	if err == nil {
		raw, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr == nil {
			if om, parseErr := parseMeta(raw); parseErr == nil {
				return contracts.ObjectInfo{
					Key: key, Size: om.Size, ContentType: om.ContentType, LastModified: om.LastModified,
				}, nil
			}
		}
	}
	return contracts.ObjectInfo{}, contracts.ErrNotFound
}

func (p *Provider) List(ctx context.Context, prefix string) ([]contracts.ObjectInfo, error) {
	cfg := p.Config()
	if cfg.Dedup {
		items, err := p.backend.List(ctx, p.backendKey("meta/"+prefix))
		if err != nil {
			return nil, err
		}
		metaPrefix := p.backendKey("meta/")
		out := make([]contracts.ObjectInfo, 0, len(items))
		for _, it := range items {
			logical := it.Key
			logical = strings.TrimPrefix(logical, metaPrefix)
			info, err := p.Stat(ctx, logical)
			if err != nil {
				continue
			}
			out = append(out, info)
		}
		return out, nil
	}
	items, err := p.backend.List(ctx, p.backendKey(prefix))
	if err != nil {
		return nil, err
	}
	bp := p.cfg.BackendPrefix
	if bp != "" {
		bp += "/"
	}
	out := make([]contracts.ObjectInfo, 0, len(items))
	for _, it := range items {
		k := it.Key
		if strings.HasPrefix(k, "sidecar/") {
			continue
		}
		if bp != "" && strings.HasPrefix(k, bp) {
			k = k[len(bp):]
		}
		info, err := p.Stat(ctx, k)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

func (p *Provider) Stream(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	blobKey, err := p.resolveBlobKey(ctx, key)
	if err != nil {
		return nil, err
	}
	rc, err := p.backend.Get(ctx, blobKey)
	if err != nil {
		return nil, err
	}
	plain, err := p.decodeBlob(rc, p.Config())
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > 0 || length > 0 {
		lim := length
		if lim <= 0 {
			lim = 1<<63 - 1
		}
		return io.NopCloser(&offsetLimitReader{src: plain, skip: offset, remain: lim}), nil
	}
	return plain, nil
}

func open(key, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}

var (
	_ contracts.StorageProvider = (*Provider)(nil)
	_ contracts.Streamable      = (*Provider)(nil)
)
