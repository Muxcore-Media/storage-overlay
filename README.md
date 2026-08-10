# storage-overlay

Transparent storage overlay for MuxCore: **encryption**, **compression**, and **content-hash dedup**.

v0.1.0 wraps an in-memory backend (swap to `storage-s3` / `storage-ceph` is a follow-up). Exposes `muxcore.storage.v1.StorageService` so core can dial it as a storage sidecar.

## Pipeline

```
Put:  plaintext → [gzip] → [AES-GCM] → [content-hash blob] → backend
Get:  backend → [resolve hash] → [AES-GCM] → [gunzip] → plaintext
```

## Run

```bash
export OVERLAY_ENCRYPT=true
export OVERLAY_COMPRESS=true
export OVERLAY_DEDUP=true
export OVERLAY_AES_KEY_HEX=$(openssl rand -hex 32)
export MUXCORE_INSECURE_DISABLE_TLS=true
go run ./cmd/module
```

gRPC `:9690`, health `:9691`.

## Settings

`encrypt`, `compress`, `dedup`, `aes_key_hex`, `prefix`.
