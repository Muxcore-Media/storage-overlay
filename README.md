# storage-overlay

Transparent storage overlay for MuxCore: **encryption**, **compression**, and **content-hash dedup**.

Backends: in-memory (default) or **MinIO / S3** (`OVERLAY_BACKEND=minio`).

## Pipeline

```
Put:  plaintext → [gzip] → content-hash (SHA-256) → [AES-GCM] → backend
Get:  backend → [resolve hash] → [AES-GCM open] → [gunzip] → plaintext
```

Dedup keys blobs by the **pre-encryption** digest (after optional gzip) so identical logical payloads share one ciphertext object even though AES-GCM uses a fresh nonce per seal.

## Run (memory)

```bash
export OVERLAY_ENCRYPT=true
export OVERLAY_COMPRESS=true
export OVERLAY_DEDUP=true
export OVERLAY_AES_KEY_HEX=$(openssl rand -hex 32)
export MUXCORE_INSECURE_DISABLE_TLS=true
go run ./cmd/module
```

## Run (MinIO backend)

```bash
# terminal A
docker run --rm -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data --console-address :9001

# terminal B
export OVERLAY_BACKEND=minio
export S3_ENDPOINT=127.0.0.1:9000
export S3_BUCKET=muxcore
export S3_ACCESS_KEY=minioadmin
export S3_SECRET_KEY=minioadmin
export S3_PATH_STYLE=true
export S3_USE_SSL=false
export OVERLAY_ENCRYPT=true
export OVERLAY_AES_KEY_HEX=$(openssl rand -hex 32)
export MUXCORE_INSECURE_DISABLE_TLS=true
go run ./cmd/module
```

(`OVERLAY_S3_*` env vars override the shared `S3_*` names when set.)

gRPC `:9690`, health `:9691`.

## Settings

`encrypt`, `compress`, `dedup`, `aes_key_hex`, `prefix`.

## Threat model & key handling

**What this protects against**

- Backend observer (disk snapshot, misconfigured object store, stolen bucket credentials) who can read stored blobs but **not** the AES key: ciphertext and metadata hashes leak object sizes / equality (same content → same blob key when dedup is on), not plaintext.
- Accidental leftover of highly compressible media on a shared volume when compress+encrypt are both enabled.

**What this does not protect against**

- Anyone who can read process memory, env, or MuxCore settings (the AES key lives there).
- Compromised module process or gRPC peer that can call Put/Get with the live provider.
- Integrity of the *index* under a malicious backend (tampered meta→hash mappings); AES-GCM detects ciphertext truncation/forgery for a given key but cannot prove the logical key pointed at the right hash.
- Key compromise retroactively: all historical blobs sealed with that key are readable.

**Key handling (local / laptop)**

| Source | Behavior |
|--------|----------|
| `OVERLAY_AES_KEY_HEX` | Preferred. Hex of 16, 24, or 32 bytes (AES-128/192/256). Generate with `openssl rand -hex 32`. |
| Setting `aes_key_hex` | Same bytes via admin settings UI; stored/displayed as a **secret** (masked). Masked placeholder `********` is ignored on update so re-saves do not wipe the key. |
| Built-in default | If neither env nor config supplies a key, the module uses a **fixed demo key** (`0x11` × 32). **Do not use that default outside local demos.** Enable encrypt only after setting a real key. |

Operational notes:

- Treat `OVERLAY_AES_KEY_HEX` like a password: keep it out of git, shell history where possible, and shared logs.
- Changing the key without re-writing objects makes existing ciphertext unreadable; there is no automatic re-key.
- Dedup meta stores content hashes in the clear on the backend; hashes are not secret and can reveal which logical keys share bytes.
- Memory backend is for tests and single-process demos. **MinIO / S3** (`OVERLAY_BACKEND=minio`) is the durable at-rest path for laptop demos.

## Tests

```bash
make test
```

- Memory encrypt/compress/dedup round-trips
- gofakes3-backed MinIO-shaped overlay round-trip
- Optional real MinIO Docker smoke (skips without Docker)
