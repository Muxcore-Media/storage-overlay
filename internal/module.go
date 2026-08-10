package internal

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc"

	"github.com/Muxcore-Media/core/pkg/contracts"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
	"github.com/Muxcore-Media/storage-overlay/internal/server"
)

type Module struct {
	id       string
	grpcAddr string
	httpAddr string

	cfgMu    sync.RWMutex
	encrypt  bool
	compress bool
	dedup    bool
	aesKey   []byte
	prefix   string

	provider *overlay.Provider
	srv      *server.Server
	grpcSrv  *grpc.Server
	lis      net.Listener
	httpSrv  *http.Server
}

type Config struct {
	ID       string
	Encrypt  bool
	Compress bool
	Dedup    bool
	AESKey   []byte
	Prefix   string
	GRPCAddr string
	HTTPAddr string
}

func NewModule(cfg Config) *Module {
	if cfg.ID == "" {
		cfg.ID = "storage-overlay"
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = ":9690"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":9691"
	}
	if v := os.Getenv("OVERLAY_ENCRYPT"); v != "" {
		cfg.Encrypt = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("OVERLAY_COMPRESS"); v != "" {
		cfg.Compress = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("OVERLAY_DEDUP"); v != "" {
		cfg.Dedup = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("OVERLAY_AES_KEY_HEX"); v != "" {
		if b, err := hex.DecodeString(v); err == nil {
			cfg.AESKey = b
		}
	}
	if len(cfg.AESKey) == 0 {
		cfg.AESKey = bytesRepeat(0x11, 32)
	}
	if v := os.Getenv("OVERLAY_PREFIX"); v != "" {
		cfg.Prefix = v
	}
	if v := os.Getenv("STORAGE_OVERLAY_GRPC_ADDR"); v != "" {
		cfg.GRPCAddr = v
	}
	if v := os.Getenv("STORAGE_OVERLAY_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	// Defaults suitable for a useful scaffold demo.
	if os.Getenv("OVERLAY_ENCRYPT") == "" && os.Getenv("OVERLAY_COMPRESS") == "" && os.Getenv("OVERLAY_DEDUP") == "" {
		cfg.Compress = true
	}
	return &Module{
		id: cfg.ID, grpcAddr: cfg.GRPCAddr, httpAddr: cfg.HTTPAddr,
		encrypt: cfg.Encrypt, compress: cfg.Compress, dedup: cfg.Dedup,
		aesKey: append([]byte(nil), cfg.AESKey...), prefix: cfg.Prefix,
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func (m *Module) Info() contracts.ModuleInfo {
	return contracts.ModuleInfo{
		ID: m.id, Name: "Storage Overlay", Version: "0.1.0",
		Roles:        []string{"storage", "infrastructure"},
		Description:  "Storage overlay: encryption, compression, content-hash dedup",
		Author:       "MuxCore",
		Capabilities: []string{"storage", "storage.overlay", "settings"},
		Contracts: []contracts.ContractDeclaration{
			{Repo: "github.com/Muxcore-Media/core/pkg/contracts", Interface: "StorageProvider", Version: "v0.5.4"},
		},
		MinCoreVersion: "0.5.4",
		HTTPAddr:       m.grpcAddr,
	}
}

func (m *Module) overlayConfig() overlay.Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return overlay.Config{
		Encrypt: m.encrypt, Compress: m.compress, Dedup: m.dedup,
		AESKey: append([]byte(nil), m.aesKey...), BackendPrefix: m.prefix,
	}
}

func (m *Module) Init(ctx context.Context) error {
	p, err := overlay.New(m.overlayConfig(), nil)
	if err != nil {
		return err
	}
	m.provider = p
	lis, err := net.Listen("tcp", m.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", m.grpcAddr, err)
	}
	m.lis = lis
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	m.httpSrv = &http.Server{Addr: m.httpAddr, Handler: mux}
	slog.Info("storage-overlay initialized", "encrypt", m.encrypt, "compress", m.compress, "dedup", m.dedup, "grpc", m.grpcAddr)
	return nil
}

func (m *Module) Start(ctx context.Context) error {
	m.grpcSrv = grpc.NewServer()
	m.srv = server.Register(m.grpcSrv, m.provider)
	modulesdk.RegisterSettings(m.grpcSrv, m.id, m)
	go func() {
		slog.Info("storage-overlay gRPC started", "addr", m.grpcAddr)
		if err := m.grpcSrv.Serve(m.lis); err != nil {
			slog.Error("gRPC serve", "error", err)
		}
	}()
	go func() {
		slog.Info("storage-overlay HTTP started", "addr", m.httpAddr)
		if err := m.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP serve", "error", err)
		}
	}()
	return nil
}

func (m *Module) Stop(ctx context.Context) error {
	if m.httpSrv != nil {
		_ = m.httpSrv.Shutdown(ctx)
	}
	if m.grpcSrv != nil {
		m.grpcSrv.GracefulStop()
	}
	return nil
}

func (m *Module) Health(ctx context.Context) error {
	if m.provider == nil {
		return fmt.Errorf("not initialized")
	}
	return nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		_, err := strconv.ParseBool(v)
		return false, fmt.Errorf("invalid bool %q: %w", v, err)
	}
}
