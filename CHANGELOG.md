# Changelog

## [Unreleased]

### Added

- MinIO / S3 backend via `OVERLAY_BACKEND=minio|s3` + `S3_*` / `OVERLAY_S3_*` env
- gofakes3 + optional MinIO Docker overlay round-trip tests

## [0.1.0] — 2026-08-10

### Added

- Overlay StorageProvider: AES-GCM encrypt, gzip compress, content-hash dedup
- In-memory backend (scaffold)
- gRPC StorageService `:9690` + health `:9691`
- SettingsProvider for overlay toggles
