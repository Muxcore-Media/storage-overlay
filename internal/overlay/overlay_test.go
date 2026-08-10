package overlay_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func TestEncryptCompressRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	p, err := overlay.New(overlay.Config{Encrypt: true, Compress: true, AESKey: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := bytes.Repeat([]byte("muxcore-overlay-"), 64)
	if err := p.Put(ctx, "a/b.bin", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}
	rc, err := p.Get(ctx, "a/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("mismatch len=%d", len(got))
	}
}

func TestDedupSharesBlob(t *testing.T) {
	p, err := overlay.New(overlay.Config{Dedup: true, Compress: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := []byte("same-payload-twice")
	if err := p.Put(ctx, "k1", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := p.Put(ctx, "k2", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	// After transforms, both keys resolve; delete one and other still works.
	if err := p.Delete(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	rc, err := p.Get(ctx, "k2")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q", got)
	}
	sum := sha256.Sum256(body)
	_ = hex.EncodeToString(sum[:]) // keep import used if transform changes hash path
}

func TestStreamRange(t *testing.T) {
	p, err := overlay.New(overlay.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := []byte("0123456789")
	_ = p.Put(ctx, "n", bytes.NewReader(body), int64(len(body)))
	rc, err := p.Stream(ctx, "n", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "2345" {
		t.Fatalf("%q", got)
	}
}
