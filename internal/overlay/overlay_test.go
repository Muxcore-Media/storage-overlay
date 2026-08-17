package overlay_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func readAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEncryptRoundTrip_CiphertextNotPlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	backend := overlay.NewMemBackend()
	p, err := overlay.New(overlay.Config{Encrypt: true, AESKey: key, BackendPrefix: "ov"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := []byte("secret-payload-must-not-appear-raw")
	if err := p.Put(ctx, "doc.txt", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}

	raw, err := backend.Get(ctx, "ov/doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	cipher := readAll(t, raw)
	if bytes.Equal(cipher, plain) {
		t.Fatal("backend stored plaintext; expected AES-GCM ciphertext")
	}
	if bytes.Contains(cipher, plain) {
		t.Fatal("plaintext substring leaked into ciphertext blob")
	}

	got := readAll(t, mustGet(t, p, ctx, "doc.txt"))
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch got=%q", got)
	}
}

func TestCompressRoundTrip_ShrinksAndRestores(t *testing.T) {
	backend := overlay.NewMemBackend()
	p, err := overlay.New(overlay.Config{Compress: true, BackendPrefix: "ov"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := bytes.Repeat([]byte("AAAA"), 4096)
	if err := p.Put(ctx, "big.bin", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}

	raw, err := backend.Get(ctx, "ov/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	stored := readAll(t, raw)
	if len(stored) >= len(plain) {
		t.Fatalf("expected gzip to shrink %d → got stored %d", len(plain), len(stored))
	}
	if stored[0] != 0x1f || stored[1] != 0x8b {
		t.Fatalf("stored blob missing gzip magic: %x", stored[:2])
	}
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatal(err)
	}
	ungzip, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ungzip, plain) {
		t.Fatal("backend gzip payload does not round-trip via stdlib gunzip")
	}

	got := readAll(t, mustGet(t, p, ctx, "big.bin"))
	if !bytes.Equal(got, plain) {
		t.Fatal("overlay Get mismatch after compress")
	}
}

func TestDedupSharesSingleBlob(t *testing.T) {
	backend := overlay.NewMemBackend()
	p, err := overlay.New(overlay.Config{Dedup: true, Compress: true, BackendPrefix: "ov"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := bytes.Repeat([]byte("same-payload-"), 32)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(body)
	_ = zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	wantHash := hex.EncodeToString(sum[:])
	blobKey := "ov/blobs/" + wantHash

	if err := p.Put(ctx, "k1", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, "k2", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}

	ok, err := backend.Exists(ctx, blobKey)
	if err != nil || !ok {
		t.Fatalf("expected shared blob %s exists=%v err=%v", blobKey, ok, err)
	}
	for _, meta := range []string{"ov/meta/k1", "ov/meta/k2"} {
		rc, err := backend.Get(ctx, meta)
		if err != nil {
			t.Fatal(err)
		}
		h := string(readAll(t, rc))
		if h != wantHash {
			t.Fatalf("%s hash=%q want %q", meta, h, wantHash)
		}
	}

	items, err := backend.List(ctx, "ov/blobs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 blob after two identical puts, got %d", len(items))
	}

	if err := p.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	ok, err = backend.Exists(ctx, blobKey)
	if err != nil || !ok {
		t.Fatal("blob deleted while k2 still references it")
	}
	got := readAll(t, mustGet(t, p, ctx, "k2"))
	if !bytes.Equal(got, body) {
		t.Fatalf("k2 after k1 delete: got %q", got)
	}

	if err := p.Delete(ctx, "k2"); err != nil {
		t.Fatal(err)
	}
	ok, err = backend.Exists(ctx, blobKey)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("blob should be removed when last ref is deleted")
	}
}

func TestEncryptCompressDedupRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x7a}, 32)
	backend := overlay.NewMemBackend()
	p, err := overlay.New(overlay.Config{
		Encrypt: true, Compress: true, Dedup: true,
		AESKey: key, BackendPrefix: "ov",
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := bytes.Repeat([]byte("muxcore-overlay-"), 128)

	if err := p.Put(ctx, "a/b.bin", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, "c/d.bin", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}

	blobs, err := backend.List(ctx, "ov/blobs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 {
		t.Fatalf("encrypt+dedup should share one blob, got %d", len(blobs))
	}
	raw := readAll(t, mustBackendGet(t, backend, ctx, blobs[0].Key))
	if bytes.Contains(raw, []byte("muxcore-overlay-")) {
		t.Fatal("compressed plaintext leaked into encrypted blob")
	}
	if raw[0] == 0x1f && raw[1] == 0x8b {
		t.Fatal("blob looks like bare gzip; expected AES-GCM wrapping")
	}

	for _, k := range []string{"a/b.bin", "c/d.bin"} {
		got := readAll(t, mustGet(t, p, ctx, k))
		if !bytes.Equal(got, plain) {
			t.Fatalf("%s round-trip mismatch", k)
		}
	}

	wrong, err := overlay.New(overlay.Config{
		Encrypt: true, Compress: true, Dedup: true,
		AESKey: bytes.Repeat([]byte{0x00}, 32), BackendPrefix: "ov",
	}, backend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Get(ctx, "a/b.bin"); err == nil {
		t.Fatal("expected decrypt failure with wrong AES key")
	}
}

func TestStreamRange(t *testing.T) {
	p, err := overlay.New(overlay.Config{Encrypt: true, Compress: true, AESKey: bytes.Repeat([]byte{1}, 16)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := []byte("0123456789")
	if err := p.Put(ctx, "n", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	rc, err := p.Stream(ctx, "n", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, rc)
	if string(got) != "2345" {
		t.Fatalf("%q", got)
	}
}

func mustGet(t *testing.T, p *overlay.Provider, ctx context.Context, key string) io.ReadCloser {
	t.Helper()
	rc, err := p.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func mustBackendGet(t *testing.T, b *overlay.MemBackend, ctx context.Context, key string) io.ReadCloser {
	t.Helper()
	rc, err := b.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}
