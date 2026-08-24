package overlay

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Muxcore-Media/core/pkg/contracts"
)

// Config controls overlay transforms applied before the backend store.
type Config struct { //nolint:govet // fieldalignment: overlay flags grouped for readability
	Encrypt       bool
	Compress      bool
	Dedup         bool
	AESKey        []byte // 16/24/32 bytes when Encrypt
	BackendPrefix string
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
	buf, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if err := m.Put(ctx, dst, bytes.NewReader(buf), int64(len(buf))); err != nil {
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

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

var _ contracts.StorageProvider = (*MemBackend)(nil)

// Provider wraps a backend StorageProvider with optional encrypt/compress/dedup.
type Provider struct { //nolint:govet // fieldalignment: mutex and config grouped for readability
	mu      sync.RWMutex
	cfg     Config
	backend contracts.StorageProvider
	// logical key -> content-addressed blob key (when Dedup)
	index map[string]string
	// content hash -> refcount
	refs map[string]int
}

// New builds an overlay Provider. backend may be nil (uses MemBackend).
func New(cfg Config, backend contracts.StorageProvider) (*Provider, error) {
	if cfg.Encrypt {
		n := len(cfg.AESKey)
		if n != 16 && n != 24 && n != 32 {
			return nil, fmt.Errorf("AES key must be 16, 24, or 32 bytes (got %d)", n)
		}
	}
	if backend == nil {
		backend = NewMemBackend()
	}
	return &Provider{
		cfg:     cfg,
		backend: backend,
		index:   map[string]string{},
		refs:    map[string]int{},
	}, nil
}

func (p *Provider) Config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *Provider) ReplaceConfig(cfg Config) error {
	if cfg.Encrypt {
		n := len(cfg.AESKey)
		if n != 16 && n != 24 && n != 32 {
			return fmt.Errorf("AES key must be 16, 24, or 32 bytes (got %d)", n)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
	return nil
}

func (p *Provider) backendKey(logical string) string {
	prefix := p.cfg.BackendPrefix
	if prefix == "" {
		return logical
	}
	return prefix + "/" + logical
}

func (p *Provider) transformOut(plain []byte) ([]byte, string, error) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	data := plain
	if cfg.Compress {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(data); err != nil {
			return nil, "", err
		}
		if err := zw.Close(); err != nil {
			return nil, "", err
		}
		data = buf.Bytes()
	}
	// Hash the pre-encryption payload so content-addressed dedup works with AES-GCM
	// (random nonces would otherwise make identical plaintext non-deduplicable).
	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])
	if cfg.Encrypt {
		out, err := seal(cfg.AESKey, data)
		if err != nil {
			return nil, "", err
		}
		data = out
	}
	return data, hashHex, nil
}

func (p *Provider) transformIn(blob []byte) ([]byte, error) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	data := blob
	if cfg.Encrypt {
		out, err := open(cfg.AESKey, data)
		if err != nil {
			return nil, err
		}
		data = out
	}
	if cfg.Compress {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, err
		}
		data = out
	}
	return data, nil
}

func (p *Provider) Put(ctx context.Context, key string, data io.Reader, size int64) error {
	plain, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	blob, hash, err := p.transformOut(plain)
	if err != nil {
		return err
	}

	p.mu.RLock()
	dedup := p.cfg.Dedup
	p.mu.RUnlock()

	if dedup {
		blobKey := p.backendKey("blobs/" + hash)
		p.mu.Lock()
		prev, had := p.index[key]
		if !had || prev != hash {
			ok, err := p.backend.Exists(ctx, blobKey)
			if err != nil {
				p.mu.Unlock()
				return err
			}
			if !ok {
				if err := p.backend.Put(ctx, blobKey, bytes.NewReader(blob), int64(len(blob))); err != nil {
					p.mu.Unlock()
					return err
				}
			}
			p.refs[hash]++
			if had {
				p.refs[prev]--
				if p.refs[prev] <= 0 {
					delete(p.refs, prev)
					_ = p.backend.Delete(ctx, p.backendKey("blobs/"+prev))
				}
			}
			p.index[key] = hash
		}
		p.mu.Unlock()
		metaKey := p.backendKey("meta/" + key)
		return p.backend.Put(ctx, metaKey, bytes.NewReader([]byte(hash)), int64(len(hash)))
	}

	return p.backend.Put(ctx, p.backendKey(key), bytes.NewReader(blob), int64(len(blob)))
}

func (p *Provider) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	blobKey, err := p.resolve(ctx, key)
	if err != nil {
		return nil, err
	}
	rc, err := p.backend.Get(ctx, blobKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	blob, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	plain, err := p.transformIn(blob)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(plain)), nil
}

func (p *Provider) resolve(ctx context.Context, key string) (string, error) {
	p.mu.RLock()
	dedup := p.cfg.Dedup
	hash, ok := p.index[key]
	p.mu.RUnlock()
	if dedup {
		if !ok {
			// try meta from backend
			rc, err := p.backend.Get(ctx, p.backendKey("meta/"+key))
			if err != nil {
				return "", err
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return "", err
			}
			hash = string(b)
			p.mu.Lock()
			p.index[key] = hash
			p.refs[hash]++
			p.mu.Unlock()
		}
		return p.backendKey("blobs/" + hash), nil
	}
	return p.backendKey(key), nil
}

func (p *Provider) Delete(ctx context.Context, key string) error {
	p.mu.RLock()
	dedup := p.cfg.Dedup
	p.mu.RUnlock()
	if !dedup {
		return p.backend.Delete(ctx, p.backendKey(key))
	}
	p.mu.Lock()
	hash, ok := p.index[key]
	if ok {
		delete(p.index, key)
		p.refs[hash]--
		if p.refs[hash] <= 0 {
			delete(p.refs, hash)
			_ = p.backend.Delete(ctx, p.backendKey("blobs/"+hash))
		}
	}
	p.mu.Unlock()
	_ = p.backend.Delete(ctx, p.backendKey("meta/"+key))
	return nil
}

func (p *Provider) Move(ctx context.Context, src, dst string) error {
	rc, err := p.Get(ctx, src)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if err := p.Put(ctx, dst, bytes.NewReader(buf), int64(len(buf))); err != nil {
		return err
	}
	return p.Delete(ctx, src)
}

func (p *Provider) Exists(ctx context.Context, key string) (bool, error) {
	p.mu.RLock()
	dedup := p.cfg.Dedup
	_, ok := p.index[key]
	p.mu.RUnlock()
	if dedup && ok {
		return true, nil
	}
	if dedup {
		return p.backend.Exists(ctx, p.backendKey("meta/"+key))
	}
	return p.backend.Exists(ctx, p.backendKey(key))
}

func (p *Provider) Stat(ctx context.Context, key string) (contracts.ObjectInfo, error) {
	rc, err := p.Get(ctx, key)
	if err != nil {
		return contracts.ObjectInfo{}, err
	}
	defer func() { _ = rc.Close() }()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return contracts.ObjectInfo{}, err
	}
	return contracts.ObjectInfo{Key: key, Size: int64(len(buf)), LastModified: time.Now().UTC()}, nil
}

func (p *Provider) List(ctx context.Context, prefix string) ([]contracts.ObjectInfo, error) {
	p.mu.RLock()
	dedup := p.cfg.Dedup
	p.mu.RUnlock()
	if dedup {
		items, err := p.backend.List(ctx, p.backendKey("meta/"+prefix))
		if err != nil {
			return nil, err
		}
		metaPrefix := p.backendKey("meta/")
		out := make([]contracts.ObjectInfo, 0, len(items))
		for _, it := range items {
			logical := it.Key
			if hasPrefix(logical, metaPrefix) {
				logical = logical[len(metaPrefix):]
			}
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
		if bp != "" && hasPrefix(k, bp) {
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
	rc, err := p.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(buf)) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	end := int64(len(buf))
	if length > 0 && offset+length < end {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(buf[offset:end])), nil
}

func seal(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
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
