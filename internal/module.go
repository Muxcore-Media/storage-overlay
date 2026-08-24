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
	"time"

	"google.golang.org/grpc"

	"github.com/Muxcore-Media/core/pkg/contracts"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
	"github.com/Muxcore-Media/storage-overlay/internal/server"
)

type Module struct { //nolint:govet // fieldalignment: lifecycle fields grouped for readability
	id       string
	grpcAddr string
	httpAddr string

	cfgMu    sync.RWMutex
	encrypt  bool
	compress bool
	dedup    bool
	aesKey   []byte
	prefix   string

	backendKind string // memory | s3 | minio
	s3          overlay.S3Config

	provider *overlay.Provider
	srv      *server.Server
	grpcSrv  *grpc.Server
	lis      net.Listener
	httpSrv  *http.Server
}

type Config struct { //nolint:govet // fieldalignment: config fields grouped for readability
	ID       string
	Encrypt  bool
	Compress bool
	Dedup    bool
	AESKey   []byte
	Prefix   string
	GRPCAddr string
	HTTPAddr string

	Backend string // memory (default) | s3 | minio
	S3      overlay.S3Config
}

func NewModule(cfg Config) *Module {
	cfg = applyOverlayDefaults(cfg)
	cfg = applyOverlayEnv(cfg)
	cfg = applyS3Env(cfg)
	return &Module{
		id: cfg.ID, grpcAddr: cfg.GRPCAddr, httpAddr: cfg.HTTPAddr,
		encrypt: cfg.Encrypt, compress: cfg.Compress, dedup: cfg.Dedup,
		aesKey: append([]byte(nil), cfg.AESKey...), prefix: cfg.Prefix,
		backendKind: cfg.Backend, s3: cfg.S3,
	}
}

func applyOverlayDefaults(cfg Config) Config {
	if cfg.ID == "" {
		cfg.ID = "storage-overlay"
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = ":9690"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":9691"
	}
	if len(cfg.AESKey) == 0 {
		cfg.AESKey = bytesRepeat(0x11, 32)
	}
	if cfg.Backend == "" {
		cfg.Backend = "memory"
	}
	if os.Getenv("OVERLAY_ENCRYPT") == "" && os.Getenv("OVERLAY_COMPRESS") == "" && os.Getenv("OVERLAY_DEDUP") == "" {
		cfg.Compress = true
	}
	return cfg
}

func applyOverlayEnv(cfg Config) Config {
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
	if v := os.Getenv("OVERLAY_PREFIX"); v != "" {
		cfg.Prefix = v
	}
	if v := os.Getenv("STORAGE_OVERLAY_GRPC_ADDR"); v != "" {
		cfg.GRPCAddr = v
	}
	if v := os.Getenv("STORAGE_OVERLAY_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("OVERLAY_BACKEND"); v != "" {
		cfg.Backend = strings.ToLower(strings.TrimSpace(v))
	}
	return cfg
}

func applyS3Env(cfg Config) Config {
	if v := firstEnv("OVERLAY_S3_ENDPOINT", "S3_ENDPOINT"); v != "" {
		cfg.S3.Endpoint = v
	}
	if v := firstEnv("OVERLAY_S3_BUCKET", "S3_BUCKET"); v != "" {
		cfg.S3.Bucket = v
	}
	if v := firstEnv("OVERLAY_S3_ACCESS_KEY", "S3_ACCESS_KEY"); v != "" {
		cfg.S3.AccessKey = v
	}
	if v := firstEnv("OVERLAY_S3_SECRET_KEY", "S3_SECRET_KEY"); v != "" {
		cfg.S3.SecretKey = v
	}
	if v := firstEnv("OVERLAY_S3_PREFIX", "S3_PREFIX"); v != "" {
		cfg.S3.Prefix = v
	}
	if v := firstEnv("OVERLAY_S3_REGION", "S3_REGION"); v != "" {
		cfg.S3.Region = v
	}
	if v := firstEnv("OVERLAY_S3_USE_SSL", "S3_USE_SSL"); v != "" {
		cfg.S3.UseSSL = v == "1" || strings.EqualFold(v, "true")
	}
	pathStyle := true
	if v := firstEnv("OVERLAY_S3_PATH_STYLE", "S3_PATH_STYLE"); v != "" {
		pathStyle = v == "1" || strings.EqualFold(v, "true")
	}
	cfg.S3.PathStyle = pathStyle
	return cfg
}

func firstEnv(primary, fallback string) string {
	if v := os.Getenv(primary); v != "" {
		return v
	}
	return os.Getenv(fallback)
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
	var backend contracts.StorageProvider
	switch strings.ToLower(m.backendKind) {
	case "", "memory", "mem":
		backend = nil // overlay.New uses MemBackend
	case "s3", "minio":
		s3, err := overlay.NewS3Backend(m.s3)
		if err != nil {
			return fmt.Errorf("overlay s3 backend: %w", err)
		}
		if err := s3.EnsureBucket(ctx); err != nil {
			return fmt.Errorf("overlay s3 ensure bucket: %w", err)
		}
		backend = s3
	default:
		return fmt.Errorf("unknown OVERLAY_BACKEND %q (want memory|s3|minio)", m.backendKind)
	}
	p, err := overlay.New(m.overlayConfig(), backend)
	if err != nil {
		return err
	}
	m.provider = p
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", m.grpcAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", m.grpcAddr, err)
	}
	m.lis = lis
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	m.httpSrv = &http.Server{
		Addr:              m.httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("storage-overlay initialized",
		"backend", m.backendKind, "encrypt", m.encrypt, "compress", m.compress, "dedup", m.dedup, "grpc", m.grpcAddr)
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
