# storage-overlay

Transparent storage overlay for MuxCore: **encryption**, **compression**, and **content-hash dedup** over an upstream `StorageProvider` or a laptop fallback backend.

Primary mode: wrap another mesh module (`OVERLAY_UPSTREAM=storage-s3`). Fallbacks: **MinIO / S3** (`OVERLAY_BACKEND=minio`) or in-memory (`OVERLAY_ALLOW_MEMORY=1` for tests only).

## Pipeline

```
Put:  plaintext → [gzip] → content-hash (SHA-256) → [chunked AES-GCM] → backend
Get:  backend → [chunk decrypt] → [gunzip] → plaintext (streamed; no full-blob ReadAll)
```

Dedup keys blobs by the **pre-encryption** digest (after optional gzip). Identical logical payloads share one ciphertext object.

## Environment

| Variable | Purpose |
|----------|---------|
| `OVERLAY_UPSTREAM` | Upstream storage module id (e.g. `storage-s3`). Requires `MUXCORE_GRPC_ADDR`. |
| `OVERLAY_BACKEND` | `upstream` (via `OVERLAY_UPSTREAM`), `minio`/`s3`, or `memory` |
| `OVERLAY_ALLOW_MEMORY` | Must be `1` to allow ephemeral memory backend (dev/tests) |
| `OVERLAY_ENCRYPT` | `true`/`1` to enable AES-GCM |
| `OVERLAY_COMPRESS` | `true`/`1` for gzip (default on when unset) |
| `OVERLAY_DEDUP` | `true`/`1` for content-hash dedup |
| `OVERLAY_AES_KEY_HEX` | **Required** when encrypt is on — 16/24/32-byte key as hex. The built-in demo key is **refused**. |
| `OVERLAY_PREFIX` | Optional key prefix on the backend |
| `OVERLAY_S3_*` / `S3_*` | MinIO/S3 endpoint, bucket, creds (when `OVERLAY_BACKEND=minio`) |
| `STORAGE_OVERLAY_GRPC_ADDR` | gRPC listen (default `:9690`) |
| `STORAGE_OVERLAY_HTTP_ADDR` | HTTP `/health` (default `:9691`) |

## Run (wrap upstream storage-s3)

```bash
export MUXCORE_GRPC_ADDR=127.0.0.1:9090
export MUXCORE_INSECURE_DISABLE_TLS=true
export MUXCORE_MESH_DIAL_LOCAL=true
export OVERLAY_UPSTREAM=storage-s3
export OVERLAY_ENCRYPT=true
export OVERLAY_AES_KEY_HEX=$(openssl rand -hex 32)
go run ./cmd/module
```

## Run (MinIO backend — durable laptop path)

```bash
# terminal A (generate credentials; no default)
export MINIO_ROOT_USER="muxcore-$(openssl rand -hex 4)" MINIO_ROOT_PASSWORD="$(openssl rand -hex 16)"
docker run --rm -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER -e MINIO_ROOT_PASSWORD \
  minio/minio server /data --console-address :9001

# terminal B
export OVERLAY_BACKEND=minio
export S3_ENDPOINT=127.0.0.1:9000
export S3_BUCKET=muxcore
export S3_ACCESS_KEY="$MINIO_ROOT_USER"
export S3_SECRET_KEY="$MINIO_ROOT_PASSWORD"
export S3_PATH_STYLE=true
export S3_USE_SSL=false
export OVERLAY_ENCRYPT=true
export OVERLAY_AES_KEY_HEX=$(openssl rand -hex 32)
export MUXCORE_INSECURE_DISABLE_TLS=true
go run ./cmd/module
```

## MVP stack (`_mvp/run-host.sh`)

`MVP_ENABLE_STORAGE_OVERLAY=1` requires `OVERLAY_AES_KEY_HEX`, defaults to MinIO backend (`OVERLAY_BACKEND=minio`), and enables encrypt/compress/dedup. Memory backend is blocked unless `OVERLAY_ALLOW_MEMORY=1`.

## Settings

`backend`, `upstream`, `encrypt`, `compress`, `dedup`, `aes_key_hex`, `prefix`, plus S3 fields when backend is `minio`/`s3`.

Runtime toggles of `encrypt` / `compress` / `dedup` are **rejected** (on-disk layout). Secret updates accept masked `********` without wiping the key.

## Threat model & key handling

**What this protects against**

- Backend observer who can read stored blobs but **not** the AES key: ciphertext and metadata hashes leak object sizes / equality (same content → same blob key when dedup is on), not plaintext.

**What this does not protect against**

- Anyone who can read process memory, env, or MuxCore settings.
- Compromised module process or gRPC peer that can call Put/Get with the live provider.

**Key handling**

| Source | Behavior |
|--------|----------|
| `OVERLAY_AES_KEY_HEX` | Preferred. Hex of 16, 24, or 32 bytes. Generate with `openssl rand -hex 32`. |
| Setting `aes_key_hex` | Same bytes via admin settings; masked on display. |
| Built-in demo key (`0x11`×32) | Used only when encrypt is **off**. **Init/UpdateSetting refuse it when encrypt is on.** |

## Tests

```bash
make test
```

- Streaming encrypt/compress/dedup round-trips
- Durable dedup refcounts across provider restart
- gRPC bufconn Put/Get/Delete/Stat/range
- gofakes3-backed MinIO-shaped overlay round-trip
