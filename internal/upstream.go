package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/Muxcore-Media/core/pkg/contracts"
	storagev1 "github.com/Muxcore-Media/core/proto/gen/muxcore/storage/v1"
	"github.com/Muxcore-Media/core/sdk/go/client"
)

type grpcStorageBackend struct {
	id     string
	client storagev1.StorageServiceClient
	conn   *grpc.ClientConn
}

func (g *grpcStorageBackend) Put(ctx context.Context, key string, data io.Reader, size int64) error {
	stream, err := g.client.Put(ctx)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	first := true
	for {
		n, readErr := data.Read(buf)
		if n > 0 {
			req := &storagev1.PutRequest{Chunk: append([]byte(nil), buf[:n]...)}
			if first {
				req.Key = key
				req.TotalSize = size
				first = false
			}
			if err := stream.Send(req); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if first {
		if err := stream.Send(&storagev1.PutRequest{Key: key, TotalSize: size}); err != nil {
			return err
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

func (g *grpcStorageBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	stream, err := g.client.Get(ctx, &storagev1.GetRequest{Key: key})
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if err != nil {
		return nil, mapUpstreamErr(err)
	}
	return &grpcReadCloser{first: first.GetChunk(), stream: stream}, nil
}

type grpcReadCloser struct {
	first  []byte
	stream storagev1.StorageService_GetClient
}

func (r *grpcReadCloser) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	msg, err := r.stream.Recv()
	if errors.Is(err, io.EOF) {
		return 0, io.EOF
	}
	if err != nil {
		return 0, mapUpstreamErr(err)
	}
	n := copy(p, msg.GetChunk())
	if n < len(msg.GetChunk()) {
		r.first = msg.GetChunk()[n:]
	}
	return n, nil
}

func (r *grpcReadCloser) Close() error { return nil }

func (g *grpcStorageBackend) Delete(ctx context.Context, key string) error {
	_, err := g.client.Delete(ctx, &storagev1.DeleteRequest{Key: key})
	return mapUpstreamErr(err)
}

func (g *grpcStorageBackend) Move(ctx context.Context, src, dst string) error {
	rc, err := g.Get(ctx, src)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if err := g.Put(ctx, dst, rc, -1); err != nil {
		return err
	}
	return g.Delete(ctx, src)
}

func (g *grpcStorageBackend) Exists(ctx context.Context, key string) (bool, error) {
	resp, err := g.client.Stat(ctx, &storagev1.StatRequest{Key: key})
	if err != nil {
		return false, mapUpstreamErr(err)
	}
	return resp.GetFound(), nil
}

func (g *grpcStorageBackend) Stat(ctx context.Context, key string) (contracts.ObjectInfo, error) {
	resp, err := g.client.Stat(ctx, &storagev1.StatRequest{Key: key})
	if err != nil {
		return contracts.ObjectInfo{}, mapUpstreamErr(err)
	}
	if !resp.GetFound() {
		return contracts.ObjectInfo{}, contracts.ErrNotFound
	}
	return contracts.ObjectInfo{
		Key: key, Size: resp.GetSize(), ContentType: resp.GetContentType(),
		LastModified: timeUnix(resp.GetLastModified()),
	}, nil
}

func (g *grpcStorageBackend) List(ctx context.Context, prefix string) ([]contracts.ObjectInfo, error) {
	resp, err := g.client.List(ctx, &storagev1.ListRequest{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	out := make([]contracts.ObjectInfo, 0, len(resp.GetObjects()))
	for _, o := range resp.GetObjects() {
		out = append(out, contracts.ObjectInfo{
			Key: o.GetKey(), Size: o.GetSize(), ContentType: o.GetContentType(),
			LastModified: timeUnix(o.GetLastModified()),
		})
	}
	return out, nil
}

func (g *grpcStorageBackend) Health(ctx context.Context) error {
	_, err := g.client.Capabilities(ctx, &storagev1.CapabilitiesRequest{})
	return err
}

func (g *grpcStorageBackend) Close() error {
	if g.conn != nil {
		return g.conn.Close()
	}
	return nil
}

func timeUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Now().UTC()
	}
	return time.Unix(sec, 0).UTC()
}

func mapUpstreamErr(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return contracts.ErrNotFound
	}
	return err
}

func dialUpstreamStorage(ctx context.Context, moduleID, selfID string) (contracts.StorageProvider, func(), error) {
	moduleID = strings.TrimSpace(moduleID)
	if moduleID == "" {
		return nil, nil, nil
	}
	meshAddr := strings.TrimSpace(os.Getenv("MUXCORE_GRPC_ADDR"))
	if meshAddr == "" {
		return nil, nil, fmt.Errorf("OVERLAY_UPSTREAM=%q requires MUXCORE_GRPC_ADDR", moduleID)
	}
	var opts []client.Option
	if os.Getenv("MUXCORE_INSECURE_DISABLE_TLS") == "true" || os.Getenv("MUXCORE_GRPC_INSECURE") == "true" {
		opts = append(opts, client.WithInsecure())
	}
	mc, err := client.Dial(meshAddr, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial core: %w", err)
	}
	modules, err := mc.Discovery.FindByCapability(ctx, "storage")
	if err != nil {
		_ = mc.Close()
		return nil, nil, fmt.Errorf("FindByCapability storage: %w", err)
	}
	var targetID, targetAddr string
	for _, m := range modules {
		id := m.GetId()
		if id == "" || id == selfID || id == "storage-overlay" {
			continue
		}
		if moduleID != "*" && id != moduleID {
			continue
		}
		targetID = id
		targetAddr = m.GetHttpAddr()
		if moduleID != "*" {
			break
		}
	}
	if targetID == "" {
		_ = mc.Close()
		return nil, nil, fmt.Errorf("upstream storage module %q not found", moduleID)
	}
	addr := dialPeerAddr(targetID, targetAddr)
	if addr == "" {
		_ = mc.Close()
		return nil, nil, fmt.Errorf("upstream %q has no dial address", targetID)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = mc.Close()
		return nil, nil, fmt.Errorf("dial upstream %q: %w", targetID, err)
	}
	backend := &grpcStorageBackend{
		id: targetID, client: storagev1.NewStorageServiceClient(conn), conn: conn,
	}
	slog.Info("storage-overlay upstream wired", "module", targetID, "addr", addr)
	cleanup := func() {
		_ = backend.Close()
		_ = mc.Close()
	}
	return backend, cleanup, nil
}

func dialPeerAddr(moduleID, httpAddr string) string {
	httpAddr = strings.TrimSpace(httpAddr)
	if httpAddr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(httpAddr)
	if err != nil || port == "" {
		return httpAddr
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return net.JoinHostPort(host, port)
	}
	if os.Getenv("MUXCORE_MESH_DIAL_LOCAL") == "true" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	if moduleID != "" {
		return net.JoinHostPort(moduleID, port)
	}
	return httpAddr
}
