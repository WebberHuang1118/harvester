# `pkg/coordstore` — design notes

A short design record for the in-tree compare-and-swap (CAS) object store package added to Harvester for DR coordination, plus the operator-facing `cwprobe` utility. This doc describes what's there, why it's shaped the way it is, and how downstream code (the DR controller) is expected to consume it.

---

## 1. Motivation

Cross-cluster DR (see [study/dr-s3-coordination-review.md](dr-s3-coordination-review.md)) needs a small shared object — the per-VM `owner.json` — that two clusters can read and update without ever both believing they own the same VM at the same time. The only correctness-preserving primitive for that is atomic compare-and-swap on the shared object.

Earlier iterations of this work lived as a proposed extension to `longhorn/backupstore` (in a clone under `backupstore/`), under the assumption it would be upstreamed. After review, the decision was to keep the implementation in-tree at Harvester instead of pursuing that upstream PR. The backupstore extension and its associated probe binary were reverted (commit `19adf4d` in the local backupstore clone reverts commit `b6caafe`); the same capability is now exposed as a native Harvester package.

This in-tree path also has a side benefit: backupstore's existing `BackupStoreDriver` interface stays untouched, and Harvester's DR coordination doesn't need any changes to backup-data flow.

---

## 2. Scope of the package

Three pieces:

| Path | Purpose |
|------|---------|
| [pkg/coordstore/store.go](../pkg/coordstore/store.go) | Backend-agnostic interface + sentinel errors. |
| [pkg/coordstore/s3/s3.go](../pkg/coordstore/s3/s3.go) | S3-compatible implementation. |
| [pkg/coordstore/s3/s3_test.go](../pkg/coordstore/s3/s3_test.go) | Unit tests (always run) + live tests (gated on env var). |
| [cmd/cwprobe/main.go](../cmd/cwprobe/main.go) | Operator-facing pre-deployment verification CLI. |

The interface stays narrow on purpose so additional backends (etcd, NFSv4 byte-range locks, Rancher) can be added later without changing consumers.

---

## 3. Public API

### 3.1 `coordstore.Store`

```go
type Store interface {
    Read(ctx context.Context, key string) (data []byte, version string, err error)
    Create(ctx context.Context, key string, data []byte) (version string, err error)
    Update(ctx context.Context, key string, data []byte, expectedVersion string) (newVersion string, err error)
    Delete(ctx context.Context, key string) error
    Probe(ctx context.Context, probePrefix string) error
}
```

`version` is opaque — driver-defined. For the S3 implementation it is the object's ETag. Callers must not interpret it; they pass it back to `Update` unchanged.

### 3.2 Sentinel errors

```go
var ErrPreconditionFailed = errors.New("coordstore: conditional write precondition failed")
var ErrNotFound           = errors.New("coordstore: object not found")
```

Both are constructed with stdlib `errors.New` so consumers use stdlib `errors.Is`. The two sentinels keep the "lost the race" and "object absent" cases distinguishable from generic transport errors, which is what the DR controller's bootstrap and CAS-retry loops need.

### 3.3 Why a package, not a method on backupstore

- **Independence.** The DR controller does not have to take a dependency on the entire backupstore package or its driver-registration model just to write a small JSON object.
- **Backend pluggability.** `coordstore.Store` makes no assumptions about the storage system. An etcd or NFSv4-locks implementation could be added later without changing the consumer surface.
- **Smaller blast radius.** Backupstore's `BackupStoreDriver` interface stays untouched; nothing in the existing backup-data path can regress as a result of this work.

---

## 4. S3 implementation

Two implementation details that matter:

### 4.1 v1 SDK with header injection

Harvester vendors only `aws-sdk-go` v1 (v1.55.7). AWS added `If-None-Match` / `If-Match` to `PutObject` in November 2024; the v1 SDK does not expose those fields on `PutObjectInput`. The implementation works around this by using `PutObjectRequest` (which returns the underlying `*request.Request` before sending) and setting the conditional header on the raw HTTP request:

```go
req, out := s.client.PutObjectRequest(&s3.PutObjectInput{Bucket: ..., Key: ..., Body: ...})
req.SetContext(ctx)
req.HTTPRequest.Header.Set("If-None-Match", "*")
err := req.Send()
```

This is safe because the SDK's SigV4 signing happens after the request is built but before it goes on the wire — the conditional header is included in the canonical request and signed correctly.

When/if Harvester moves to aws-sdk-go-v2, this path simplifies to setting `IfNoneMatch` / `IfMatch` directly on `PutObjectInput`. The package interface does not change.

### 4.2 Error classification

Two helpers:

```go
func isPreconditionFailed(err error) bool   // 412 / "PreconditionFailed" / "ConditionalRequestConflict"
func isNotFound(err error) bool             // 404 / NoSuchKey / NotFound
```

Both check the typed `awserr.Error` and the HTTP `awserr.RequestFailure` paths because not every S3-compatible backend surfaces the same error shape. Anything that matches gets translated into the package-level sentinel before returning to callers.

---

## 5. Operator tool: `cwprobe`

`cmd/cwprobe/main.go` is the pre-deployment verification utility. It runs every CAS check in sequence against a configured target and prints structured per-check results:

```
[PASS] probe If-None-Match enforcement — second write of identical key must be rejected
[PASS] Read(missing) returns ErrNotFound
[PASS] Create(new key) succeeds — returned version="..."
[PASS] Create(existing key) rejected
[PASS] Update(matching version) succeeds — new version="..."
[PASS] Update returns fresh version
[PASS] Update(stale version) rejected
[PASS] Read(after CAS) returns latest content+version — body="v2" version="..."
```

Exit codes:

| Code | Meaning |
|------|---------|
| `0` | All checks passed; backend is safe for CAS coordination. |
| `1` | At least one check failed; backend is NOT safe. |
| `2` | Setup error (bad URL, missing credentials, connectivity). |

### 5.1 Flags

| Flag | Purpose |
|------|---------|
| `-url` | Backup target URL, e.g. `s3://bucket@region/path`. Defaults to `$BACKUPSTORE_URL`. |
| `-prefix` | Path under the URL root for probe and CAS test objects (default `cwprobe`). |
| `-keep` | Skip cleanup. Useful for post-mortem inspection on failure. |

Credentials and endpoint configuration are read from the same env vars as Harvester's backup-target setup (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN`, `AWS_ENDPOINTS`, `AWS_CERT`).

### 5.2 Why every check, every run

The earlier iteration had a "default mode = probe only / `--full` mode = roundtrip" split. The current version always runs every check because:

- The full battery is still fast (a handful of sub-second S3 operations).
- An "extra check" — verifying that `Update` returns a *fresh* ETag — catches a real backend bug class that probe-only does not. A backend that returns the same ETag on update would defeat lease-renewal observability.
- Operators only have to learn one mode.

### 5.3 Sample usage

```bash
# Build
go build -o cwprobe ./cmd/cwprobe

# AWS S3
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
  cwprobe -url 's3://my-coordination-bucket@us-east-1/dr'

# MinIO
AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin \
AWS_ENDPOINTS=http://minio:9000 \
  cwprobe -url 's3://mybucket@pcloud/dr'

# Skip cleanup for post-mortem inspection
cwprobe -url 's3://mybucket@pcloud/dr' -keep
```

### 5.4 Verified setup

The following invocation was used to verify cwprobe end-to-end against a local bitnami/minio container (region configured as `pcloud` via `MINIO_REGION`, conditional-write support requires MinIO 2024-Q4 or newer — `bitnamilegacy/minio:2025.4.22-debian-12-r1` was used here):

```bash
go build -o /tmp/cwprobe ./cmd/cwprobe
AWS_ACCESS_KEY_ID=minioadmin \
AWS_SECRET_ACCESS_KEY=minioadmin \
AWS_ENDPOINTS=http://10.115.1.120:9000 \
/tmp/cwprobe -url 's3://mybucket@pcloud/cwprobe-test'
```

Expected output on a conformant backend:

```
Backend: s3 bucket=mybucket region=pcloud endpoint=http://10.115.1.120:9000
Prefix:  cwprobe-test/cwprobe

[PASS] probe If-None-Match enforcement — second write of identical key must be rejected
[PASS] Read(missing) returns ErrNotFound
[PASS] Create(new key) succeeds — returned version="..."
[PASS] Create(existing key) rejected
[PASS] Update(matching version) succeeds — new version="..."
[PASS] Update returns fresh version
[PASS] Update(stale version) rejected
[PASS] Read(after CAS) returns latest content+version — body="v2" version="..."

Result: 8/8 checks passed
Backend is safe for CAS-based coordination.
```

---

## 6. Tests

`pkg/coordstore/s3/s3_test.go` runs in two layers:

- **Unit tests** (always run): input validation in `New`, the empty-`expectedVersion` guard on `Update`, and the typed-error / nil-error paths in `isPreconditionFailed` / `isNotFound`.
- **Live tests** (skipped when `COORDSTORE_TEST_S3_URL` is unset): probe, Create-rejects-existing, Update-with-matching-and-stale-version, Read-missing-returns-ErrNotFound. Live tests use a dedicated `coordstore-test` subpath under the configured bucket.

Run live tests:

```bash
export COORDSTORE_TEST_S3_URL="s3://mybucket@pcloud/test-prefix"
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_ENDPOINTS=http://minio:9000   # for MinIO
go test ./pkg/coordstore/s3/...
```

---

## 7. Consumer pattern

Expected pattern for the DR controller:

```go
import (
    "github.com/harvester/harvester/pkg/coordstore"
    coords3 "github.com/harvester/harvester/pkg/coordstore/s3"
)

store, err := coords3.New(coords3.Config{
    Bucket:          "harvester-dr-control",
    Region:          "pcloud",
    Endpoint:        "http://minio:9000",
    AccessKeyID:     ...,
    SecretAccessKey: ...,
    UsePathStyle:    true,
})
if err != nil { return err }

// Probe at startup. Refuse to run if the backend is unsafe.
if err := store.Probe(ctx, "dr/_probe"); err != nil {
    return fmt.Errorf("backend not safe for DR coordination: %w", err)
}

// Bootstrap: read existing or create.
data, version, err := store.Read(ctx, "dr/vms/default/vm1/owner.json")
switch {
case errors.Is(err, coordstore.ErrNotFound):
    version, err = store.Create(ctx, "dr/vms/default/vm1/owner.json", initialOwnerJSON)
    if errors.Is(err, coordstore.ErrPreconditionFailed) {
        // Lost the race to another client that just created it — re-read.
        data, version, err = store.Read(ctx, "dr/vms/default/vm1/owner.json")
    }
case err != nil:
    return err
}

// Mutate: read-modify-CAS-write loop.
for {
    data, version, err := store.Read(ctx, key)
    if err != nil { return err }

    newData := mutate(data)

    _, err = store.Update(ctx, key, newData, version)
    if errors.Is(err, coordstore.ErrPreconditionFailed) {
        continue // someone else updated between our read and write; retry
    }
    if err != nil { return err }
    break
}
```

The probe runs once at startup. The bootstrap-or-read pattern handles initial setup races between cooperating clients. The mutate loop is the standard CAS retry loop.

---

## 8. Build verification

From the harvester repo root:

```
go build ./pkg/coordstore/... ./cmd/cwprobe/...   # passes
go vet   ./pkg/coordstore/... ./cmd/cwprobe/...   # passes
go test  ./pkg/coordstore/...                     # passes (live tests skipped without env var)
go build -o /tmp/cwprobe ./cmd/cwprobe            # passes
```

---

## 9. Open follow-ups

- **List operation.** The current interface has no listing of keys under a prefix. Not needed by the DR controller's lease/state-machine pattern, but a future consumer (e.g. an admin UI listing all protected VMs) might want it.
- **Compaction / GC.** No automatic cleanup of stale objects. Probe sentinels are auto-removed by `Probe`; CAS test objects are removed by `cwprobe` unless `-keep` is set; `owner.json` objects are owned by the DR controller's lifecycle.
- **Additional backends.** When the design eventually wants a non-S3 coordination plane (etcd, NFSv4 locks, Rancher-mediated), implement `coordstore.Store` against that backend in a sibling package (`pkg/coordstore/etcd/`, etc.) and let consumers select via the construction site. No interface changes needed.
- **aws-sdk-go-v2 migration.** When Harvester moves to v2, the header-injection workaround in [pkg/coordstore/s3/s3.go](../pkg/coordstore/s3/s3.go#L172) collapses to setting `IfNoneMatch` / `IfMatch` directly on `PutObjectInput`. The public API of the package does not change.
