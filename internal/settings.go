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
	defs := []contracts.SettingDef{
		{Key: "backend", Label: "Backend", Type: contracts.SettingTypeString, Value: m.backendKind, Description: "upstream|memory|s3|minio; OVERLAY_BACKEND", Group: "Overlay"},
		{Key: "encrypt", Label: "Encrypt", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.encrypt), Default: "false", Description: "AES-GCM at rest; OVERLAY_ENCRYPT", Group: "Overlay"},
		{Key: "compress", Label: "Compress", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.compress), Default: "true", Description: "gzip before write; OVERLAY_COMPRESS", Group: "Overlay"},
		{Key: "dedup", Label: "Dedup", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.dedup), Default: "false", Description: "Content-hash dedup; OVERLAY_DEDUP", Group: "Overlay"},
		{Key: "aes_key_hex", Label: "AES Key (hex)", Type: contracts.SettingTypeSecret, Value: modulesdk.MaskSecret(hex.EncodeToString(m.aesKey)), Description: "16/24/32-byte key hex; OVERLAY_AES_KEY_HEX", Group: "Overlay"},
		{Key: "prefix", Label: "Backend Prefix", Type: contracts.SettingTypeString, Value: m.prefix, Description: "Optional backend key prefix; OVERLAY_PREFIX", Group: "Overlay"},
		{Key: "upstream", Label: "Upstream Module", Type: contracts.SettingTypeString, Value: m.upstreamID, Description: "Storage module id; OVERLAY_UPSTREAM", Group: "Overlay"},
	}
	if m.backendKind == "s3" || m.backendKind == "minio" {
		defs = append(defs,
			contracts.SettingDef{Key: "s3_endpoint", Label: "S3 Endpoint", Type: contracts.SettingTypeString, Value: m.s3.Endpoint, Group: "S3"},
			contracts.SettingDef{Key: "s3_bucket", Label: "S3 Bucket", Type: contracts.SettingTypeString, Value: m.s3.Bucket, Group: "S3"},
			contracts.SettingDef{Key: "s3_region", Label: "S3 Region", Type: contracts.SettingTypeString, Value: m.s3.Region, Group: "S3"},
			contracts.SettingDef{Key: "s3_access_key", Label: "S3 Access Key", Type: contracts.SettingTypeSecret, Value: modulesdk.MaskSecret(m.s3.AccessKey), Group: "S3"},
			contracts.SettingDef{Key: "s3_secret_key", Label: "S3 Secret Key", Type: contracts.SettingTypeSecret, Value: modulesdk.MaskSecret(m.s3.SecretKey), Group: "S3"},
			contracts.SettingDef{Key: "s3_prefix", Label: "S3 Prefix", Type: contracts.SettingTypeString, Value: m.s3.Prefix, Group: "S3"},
			contracts.SettingDef{Key: "s3_use_ssl", Label: "S3 Use SSL", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.s3.UseSSL), Group: "S3"},
			contracts.SettingDef{Key: "s3_path_style", Label: "S3 Path Style", Type: contracts.SettingTypeBool, Value: fmt.Sprintf("%t", m.s3.PathStyle), Group: "S3"},
		)
	}
	return defs
}

// checkImmutableBool parses value and rejects it unless it equals current: the
// setting affects the on-disk layout and cannot change at runtime.
func checkImmutableBool(name, value string, current bool) error {
	b, err := parseBool(value)
	if err != nil {
		return err
	}
	if b != current {
		return fmt.Errorf("cannot change %s at runtime: would alter on-disk layout", name)
	}
	return nil
}

// parseBoolInto parses value and stores it in dst only when it is valid.
func parseBoolInto(dst *bool, value string) error {
	b, err := parseBool(value)
	if err != nil {
		return err
	}
	*dst = b
	return nil
}

// s3StringField maps a plain-string S3 setting key (or alias) to its field.
func (m *Module) s3StringField(key string) *string {
	switch key {
	case "s3_endpoint", "OVERLAY_S3_ENDPOINT", "S3_ENDPOINT":
		return &m.s3.Endpoint
	case "s3_bucket", "OVERLAY_S3_BUCKET", "S3_BUCKET":
		return &m.s3.Bucket
	case "s3_region", "OVERLAY_S3_REGION", "S3_REGION":
		return &m.s3.Region
	case "s3_prefix", "OVERLAY_S3_PREFIX", "S3_PREFIX":
		return &m.s3.Prefix
	}
	return nil
}

func (m *Module) UpdateSetting(key, value string) error {
	value = strings.TrimSpace(value)
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()

	switch key {
	case "backend", "OVERLAY_BACKEND":
		if value == "" {
			return fmt.Errorf("backend must not be empty")
		}
		m.backendKind = strings.ToLower(value)
	case "upstream", "OVERLAY_UPSTREAM":
		m.upstreamID = value
		if value != "" {
			m.backendKind = "upstream"
		}
	case "encrypt", "OVERLAY_ENCRYPT":
		if err := checkImmutableBool("encrypt", value, m.encrypt); err != nil {
			return err
		}
	case "compress", "OVERLAY_COMPRESS":
		if err := checkImmutableBool("compress", value, m.compress); err != nil {
			return err
		}
	case "dedup", "OVERLAY_DEDUP":
		if err := checkImmutableBool("dedup", value, m.dedup); err != nil {
			return err
		}
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
		if err := overlay.ValidateEncryptKey(m.encrypt, b); err != nil {
			return err
		}
		m.aesKey = b
	case "prefix", "OVERLAY_PREFIX":
		m.prefix = value
	case "s3_access_key", "OVERLAY_S3_ACCESS_KEY", "S3_ACCESS_KEY":
		if value == "********" {
			return nil
		}
		m.s3.AccessKey = value
	case "s3_secret_key", "OVERLAY_S3_SECRET_KEY", "S3_SECRET_KEY":
		if value == "********" {
			return nil
		}
		m.s3.SecretKey = value
	case "s3_use_ssl", "OVERLAY_S3_USE_SSL", "S3_USE_SSL":
		if err := parseBoolInto(&m.s3.UseSSL, value); err != nil {
			return err
		}
	case "s3_path_style", "OVERLAY_S3_PATH_STYLE", "S3_PATH_STYLE":
		if err := parseBoolInto(&m.s3.PathStyle, value); err != nil {
			return err
		}
	default:
		dst := m.s3StringField(key)
		if dst == nil {
			return fmt.Errorf("unknown setting %q", key)
		}
		*dst = value
	}
	if m.provider == nil {
		return nil
	}
	return m.provider.ReplaceConfig(overlay.Config{
		Encrypt: m.encrypt, Compress: m.compress, Dedup: m.dedup,
		AESKey: append([]byte(nil), m.aesKey...), BackendPrefix: m.prefix,
	})
}
