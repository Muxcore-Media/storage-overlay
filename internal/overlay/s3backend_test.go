package overlay_test

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func testS3Backend(t *testing.T) *overlay.S3Backend {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	ts := httptest.NewServer(faker.Server())
	t.Cleanup(ts.Close)

	endpoint := strings.TrimPrefix(ts.URL, "http://")
	st, err := overlay.NewS3Backend(overlay.S3Config{
		Endpoint:  endpoint,
		Bucket:    "muxcore",
		AccessKey: "id",
		SecretKey: "secret",
		UseSSL:    false,
		PathStyle: true,
		Prefix:    "raw",
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	if err := st.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	return st
}

func TestOverlayOnMinIOS3_EncryptRoundTrip(t *testing.T) {
	s3 := testS3Backend(t)
	key := bytes.Repeat([]byte{0x42}, 32)
	p, err := overlay.New(overlay.Config{
		Encrypt: true, Compress: true, AESKey: key, BackendPrefix: "ov",
	}, s3)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := []byte("overlay-on-minio-shaped-backend")
	if err := p.Put(ctx, "doc.txt", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatal(err)
	}

	// Ciphertext lives on the S3 backend, not plaintext.
	raw, err := s3.Get(ctx, "ov/doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := io.ReadAll(raw)
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, plain) {
		t.Fatal("plaintext leaked into S3 blob")
	}

	gotRC, err := p.Get(ctx, "doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(gotRC)
	_ = gotRC.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q", got)
	}
}
