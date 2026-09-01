package server

import (
	"context"
	"errors"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Muxcore-Media/core/pkg/contracts"
	storagev1 "github.com/Muxcore-Media/core/proto/gen/muxcore/storage/v1"
	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

// Server exposes StorageService backed by an overlay Provider.
type Server struct { //nolint:govet // fieldalignment: embedded server type first
	storagev1.UnimplementedStorageServiceServer
	mu      sync.RWMutex
	backend *overlay.Provider
}

func New(st *overlay.Provider) *Server {
	return &Server{backend: st}
}

func Register(gs *grpc.Server, st *overlay.Provider) *Server {
	s := New(st)
	storagev1.RegisterStorageServiceServer(gs, s)
	return s
}

func (s *Server) Replace(st *overlay.Provider) {
	s.mu.Lock()
	s.backend = st
	s.mu.Unlock()
}

func (s *Server) get() *overlay.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backend
}

func (s *Server) Put(stream storagev1.StorageService_PutServer) error {
	st := s.get()
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "missing key")
	}
	if err != nil {
		return err
	}
	key := first.GetKey()
	size := first.GetTotalSize()
	if key == "" {
		return status.Error(codes.InvalidArgument, "missing key")
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		defer func() { _ = pw.Close() }()
		if chunk := first.GetChunk(); len(chunk) > 0 {
			if _, werr := pw.Write(chunk); werr != nil {
				errCh <- werr
				return
			}
		}
		for {
			msg, recvErr := stream.Recv()
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if recvErr != nil {
				errCh <- recvErr
				_ = pw.CloseWithError(recvErr)
				return
			}
			if chunk := msg.GetChunk(); len(chunk) > 0 {
				if _, werr := pw.Write(chunk); werr != nil {
					errCh <- werr
					return
				}
			}
		}
		close(errCh)
	}()

	putErr := make(chan error, 1)
	go func() {
		putErr <- st.Put(stream.Context(), key, pr, size)
	}()
	if err := <-putErr; err != nil {
		return status.Errorf(codes.Internal, "put: %v", err)
	}
	if err := <-errCh; err != nil {
		return status.Errorf(codes.Internal, "put stream: %v", err)
	}
	if size <= 0 {
		if info, statErr := st.Stat(stream.Context(), key); statErr == nil {
			size = info.Size
		}
	}
	return stream.SendAndClose(&storagev1.PutResponse{Key: key, Size: size})
}

func (s *Server) Get(req *storagev1.GetRequest, stream storagev1.StorageService_GetServer) error {
	st := s.get()
	var (
		rc  io.ReadCloser
		err error
	)
	if req.GetOffset() > 0 || req.GetLength() > 0 {
		rc, err = st.Stream(stream.Context(), req.GetKey(), req.GetOffset(), req.GetLength())
	} else {
		rc, err = st.Get(stream.Context(), req.GetKey())
	}
	if err != nil {
		return mapStatus(err)
	}
	defer func() { _ = rc.Close() }()

	info, _ := st.Stat(stream.Context(), req.GetKey())
	buf := make([]byte, 32*1024)
	first := true
	for {
		n, readErr := rc.Read(buf)
		if n > 0 {
			msg := &storagev1.GetResponse{Chunk: append([]byte(nil), buf[:n]...)}
			if first {
				msg.TotalSize = info.Size
				msg.ContentType = info.ContentType
				first = false
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read: %v", readErr)
		}
	}
}

func (s *Server) Delete(ctx context.Context, req *storagev1.DeleteRequest) (*storagev1.DeleteResponse, error) {
	st := s.get()
	ok, err := st.Exists(ctx, req.GetKey())
	if err != nil {
		return nil, mapStatus(err)
	}
	if !ok {
		return &storagev1.DeleteResponse{Deleted: false}, nil
	}
	if err := st.Delete(ctx, req.GetKey()); err != nil {
		return nil, mapStatus(err)
	}
	return &storagev1.DeleteResponse{Deleted: true}, nil
}

func (s *Server) Stat(ctx context.Context, req *storagev1.StatRequest) (*storagev1.StatResponse, error) {
	info, err := s.get().Stat(ctx, req.GetKey())
	if err != nil {
		if errors.Is(err, contracts.ErrNotFound) {
			return &storagev1.StatResponse{Found: false, Key: req.GetKey()}, nil
		}
		return nil, mapStatus(err)
	}
	return &storagev1.StatResponse{
		Found:        true,
		Key:          info.Key,
		Size:         info.Size,
		ContentType:  info.ContentType,
		LastModified: info.LastModified.Unix(),
	}, nil
}

func (s *Server) List(ctx context.Context, req *storagev1.ListRequest) (*storagev1.ListResponse, error) {
	items, err := s.get().List(ctx, req.GetPrefix())
	if err != nil {
		return nil, mapStatus(err)
	}
	out := make([]*storagev1.StatResponse, 0, len(items))
	for _, it := range items {
		out = append(out, &storagev1.StatResponse{
			Found:        true,
			Key:          it.Key,
			Size:         it.Size,
			ContentType:  it.ContentType,
			LastModified: it.LastModified.Unix(),
		})
	}
	return &storagev1.ListResponse{Objects: out}, nil
}

func (s *Server) Capabilities(ctx context.Context, _ *storagev1.CapabilitiesRequest) (*storagev1.CapabilitiesResponse, error) {
	return &storagev1.CapabilitiesResponse{Capabilities: []string{"streamable", "overlay"}}, nil
}

func mapStatus(err error) error {
	if errors.Is(err, contracts.ErrNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Errorf(codes.Internal, "%v", err)
}
