package overlay_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}

func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer lis.Close()
	return lis.Addr().(*net.TCPAddr).Port
}

// TestOverlayOnRealMinIO wraps a live MinIO container when Docker is present.
func TestOverlayOnRealMinIO(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available; gofakes3 covers MinIO-shaped S3 backend")
	}

	port := freePort(t)
	name := fmt.Sprintf("storage-overlay-minio-%d", port)
	access, secret := "minioadmin", "minioadmin"

	run := exec.Command("docker", "run", "-d", "--rm",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "MINIO_ROOT_USER="+access,
		"-e", "MINIO_ROOT_PASSWORD="+secret,
		"minio/minio:latest", "server", "/data",
	)
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run minio: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "stop", "-t", "1", name).Run()
	})

	endpoint := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	var s3 *overlay.S3Backend
	for {
		s3, err = overlay.NewS3Backend(overlay.S3Config{
			Endpoint:  endpoint,
			Bucket:    "muxcore",
			AccessKey: access,
			SecretKey: secret,
			UseSSL:    false,
			PathStyle: true,
			Prefix:    "raw",
		})
		if err == nil {
			err = s3.EnsureBucket(context.Background())
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("minio not ready at %s: %v", endpoint, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	key := bytes.Repeat([]byte{0x7a}, 32)
	p, err := overlay.New(overlay.Config{
		Encrypt: true, Compress: true, Dedup: true, AESKey: key, BackendPrefix: "ov",
	}, s3)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain := []byte("minio-overlay-payload")
	if err := p.Put(ctx, "a.txt", bytes.NewReader(plain), int64(len(plain))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := p.Get(ctx, "a.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q", got)
	}
}
