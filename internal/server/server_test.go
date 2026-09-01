package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	storagev1 "github.com/Muxcore-Media/core/proto/gen/muxcore/storage/v1"
	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
	"github.com/Muxcore-Media/storage-overlay/internal/server"
)

func startOverlayServer(t *testing.T, p *overlay.Provider) storagev1.StorageServiceClient {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	server.Register(gs, p)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.GracefulStop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return storagev1.NewStorageServiceClient(conn)
}

func TestGRPCPutGetDeleteStat(t *testing.T) {
	p, err := overlay.New(overlay.Config{Compress: true}, overlay.NewMemBackend())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := startOverlayServer(t, p)
	ctx := context.Background()
	body := []byte("grpc-round-trip")

	put, err := client.Put(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := put.Send(&storagev1.PutRequest{Key: "obj.bin", TotalSize: int64(len(body)), Chunk: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := put.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}

	stat, err := client.Stat(ctx, &storagev1.StatRequest{Key: "obj.bin"})
	if err != nil || !stat.GetFound() || stat.GetSize() != int64(len(body)) {
		t.Fatalf("Stat: found=%v size=%d err=%v", stat.GetFound(), stat.GetSize(), err)
	}

	get, err := client.Get(ctx, &storagev1.GetRequest{Key: "obj.bin"})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		msg, recvErr := get.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		got = append(got, msg.GetChunk()...)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q", got)
	}

	del, err := client.Delete(ctx, &storagev1.DeleteRequest{Key: "obj.bin"})
	if err != nil || !del.GetDeleted() {
		t.Fatalf("Delete: deleted=%v err=%v", del.GetDeleted(), err)
	}
}

func TestGRPCGetRange(t *testing.T) {
	key := bytes.Repeat([]byte{0x7a}, 32)
	p, err := overlay.New(overlay.Config{Encrypt: true, Compress: true, AESKey: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := []byte("0123456789abcdef")
	if err := p.Put(ctx, "r", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	client := startOverlayServer(t, p)
	get, err := client.Get(ctx, &storagev1.GetRequest{Key: "r", Offset: 4, Length: 4})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for {
		msg, recvErr := get.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		got = append(got, msg.GetChunk()...)
	}
	if string(got) != "4567" {
		t.Fatalf("range got %q", got)
	}
}
