package internal

import (
	"context"
	"testing"

	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func TestHealthBeforeInit(t *testing.T) {
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	if err := m.Health(context.Background()); err == nil {
		t.Fatal("expected health error before Init")
	}
}

func TestInitRejectsDemoKeyWithEncrypt(t *testing.T) {
	t.Setenv("OVERLAY_ALLOW_MEMORY", "1")
	m := NewModule(Config{
		Encrypt:  true,
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	})
	if err := m.Init(context.Background()); err == nil {
		t.Fatal("expected demo key refusal")
	}
}

func TestInitMemoryWithAllow(t *testing.T) {
	t.Setenv("OVERLAY_ALLOW_MEMORY", "1")
	m := NewModule(Config{
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
}

func TestInitRejectsMemoryWithoutAllow(t *testing.T) {
	m := NewModule(Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	if err := m.Init(context.Background()); err == nil {
		t.Fatal("expected memory backend refusal")
	}
}

func TestValidateEncryptKey(t *testing.T) {
	if err := overlay.ValidateEncryptKey(true, overlay.DemoAESKey()); err == nil {
		t.Fatal("expected demo key error")
	}
}
