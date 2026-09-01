package internal

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"testing"

	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func testModule(t *testing.T) *Module {
	t.Helper()
	t.Setenv("OVERLAY_ALLOW_MEMORY", "1")
	m := NewModule(Config{
		Compress: true,
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	})
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m
}

func TestSettingsMaskSecrets(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	m := NewModule(Config{AESKey: key, GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"})
	byKey := map[string]string{}
	for _, d := range m.Settings() {
		byKey[d.Key] = d.Value
	}
	if byKey["aes_key_hex"] == hex.EncodeToString(key) {
		t.Fatal("aes key not masked")
	}
}

func TestUpdateSettingMaskNoWipe(t *testing.T) {
	m := testModule(t)
	orig := append([]byte(nil), m.aesKey...)
	if err := m.UpdateSetting("aes_key_hex", "********"); err != nil {
		t.Fatalf("UpdateSetting: %v", err)
	}
	if !bytes.Equal(m.aesKey, orig) {
		t.Fatal("masked update wiped key")
	}
}

func TestUpdateSettingInvalidAESLength(t *testing.T) {
	m := testModule(t)
	if err := m.UpdateSetting("aes_key_hex", "abcd"); err == nil {
		t.Fatal("expected invalid key length error")
	}
}

func TestUpdateSettingRejectsLayoutToggle(t *testing.T) {
	m := testModule(t)
	ctx := context.Background()
	body := []byte("layout-toggle-test")
	if err := m.provider.Put(ctx, "k", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSetting("dedup", "true"); err == nil {
		t.Fatal("expected dedup toggle rejection")
	}
	rc, err := m.provider.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("Get broken after rejected toggle")
	}
}

func TestUpdateSettingRejectsDemoKeyWhenEncrypt(t *testing.T) {
	t.Setenv("OVERLAY_ALLOW_MEMORY", "1")
	m := NewModule(Config{
		Encrypt:  true,
		AESKey:   bytes.Repeat([]byte{0x55}, 32),
		GRPCAddr: "127.0.0.1:0",
		HTTPAddr: "127.0.0.1:0",
	})
	if err := m.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if err := m.UpdateSetting("aes_key_hex", hex.EncodeToString(overlay.DemoAESKey())); err == nil {
		t.Fatal("expected demo key rejection")
	}
}
