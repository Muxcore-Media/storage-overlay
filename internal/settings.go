package internal

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Muxcore-Media/core/pkg/contracts"
	modulesdk "github.com/Muxcore-Media/core/sdk/go/module"
	"github.com/Muxcore-Media/storage-overlay/internal/overlay"
)

func (m *Module) Settings() []contracts.SettingDef {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return []contracts.SettingDef{
		{Key: "encrypt", Label: "Encrypt", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.encrypt), Default: "false", Description: "AES-GCM at rest; OVERLAY_ENCRYPT", Group: "Overlay"},
		{Key: "compress", Label: "Compress", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.compress), Default: "true", Description: "gzip before write; OVERLAY_COMPRESS", Group: "Overlay"},
		{Key: "dedup", Label: "Dedup", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.dedup), Default: "false", Description: "Content-hash dedup; OVERLAY_DEDUP", Group: "Overlay"},
		{Key: "aes_key_hex", Label: "AES Key (hex)", Type: contracts.SettingTypeSecret, Value: modulesdk.MaskSecret(hex.EncodeToString(m.aesKey)), Description: "16/24/32-byte key hex; OVERLAY_AES_KEY_HEX", Group: "Overlay"},
		{Key: "prefix", Label: "Backend Prefix", Type: contracts.SettingTypeString, Value: m.prefix, Description: "Optional backend key prefix; OVERLAY_PREFIX", Group: "Overlay"},
	}
}

func (m *Module) UpdateSetting(key, value string) error {
	value = strings.TrimSpace(value)
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	switch key {
	case "encrypt", "OVERLAY_ENCRYPT":
		b, err := parseBool(value)
		if err != nil {
			return err
		}
		m.encrypt = b
	case "compress", "OVERLAY_COMPRESS":
		b, err := parseBool(value)
		if err != nil {
			return err
		}
		m.compress = b
	case "dedup", "OVERLAY_DEDUP":
		b, err := parseBool(value)
		if err != nil {
			return err
		}
		m.dedup = b
	case "aes_key_hex", "OVERLAY_AES_KEY_HEX":
		if value == "********" {
			return nil
		}
		b, err := hex.DecodeString(value)
		if err != nil {
			return fmt.Errorf("aes_key_hex: %w", err)
		}
		if n := len(b); n != 16 && n != 24 && n != 32 {
			return fmt.Errorf("aes key must be 16/24/32 bytes")
		}
		m.aesKey = b
	case "prefix", "OVERLAY_PREFIX":
		m.prefix = value
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	if m.provider == nil {
		return nil
	}
	return m.provider.ReplaceConfig(overlay.Config{
		Encrypt: m.encrypt, Compress: m.compress, Dedup: m.dedup,
		AESKey: append([]byte(nil), m.aesKey...), BackendPrefix: m.prefix,
	})
}
