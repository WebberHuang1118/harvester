# Harvester DR via S3-only Coordination

A design sketch for cross-cluster disaster recovery in Harvester. Paired clusters share two storage targets — a backup target (data) and a DR coordination target (S3 with conditional-write support) — and reach all distributed-state agreement through those targets, with no direct cross-cluster Kubernetes API access. This document covers architecture, workflows, invariants, design considerations, and open questions to resolve before drafting a formal HEP.

---

## 1. Overview

Two Harvester clusters (`cluster-a`, `cluster-b`) participate in DR for a set of protected VMs. Both clusters share two storage targets: a **backup target** that holds VM backup data, and a **DR coordination target** (S3 with conditional-write support) that holds the small JSON ownership object the controllers use to arbitrate which cluster is currently primary. Each cluster runs an instance of `harvester-dr-controller`. Controllers do not communicate with each other's Kubernetes API; all shared state lives in the two targets.

A protected VM runs on exactly one cluster at a time (the primary). The other cluster (the secondary) holds restorable backup data and is ready to take over when the operator triggers a failover.

---

## 2. Architecture

### 2.1 Targets: data plane and coordination plane

The design uses two logically distinct shared targets. They may be the same physical S3 bucket with separate prefixes, but the architecture treats them as independent so they can be configured separately and so the coordination target can sit on a different backend from the backup target when the latter cannot honor conditional writes.

| Target | Purpose | Required capabilities |
|--------|---------|------------------------|
| **Backup target** | Holds `VMBackup` data (Longhorn volume backups + JSON metadata) consumed by `VMBackup` / `VMRestore`. | Whatever Harvester already supports — S3, NFS, CIFS, Azure Blob. No new requirements. |
| **DR coordination target** | Holds the per-VM `owner.json` object the controllers use to arbitrate ownership. | S3 with conditional-write support (verified at startup; see §5.1 and §5.12). |

The DR controller's S3 access for the coordination target is configured independently from the backup target. Concrete consequences:

- An operator with an NFS or CIFS backup target can still use DR — they point the DR controller at any S3 (or MinIO) bucket with conditional-write support, anywhere both clusters can reach.
- The coordination target can use entirely separate credentials. Useful for security boundaries — for example, the backup target may sit in one tenant or VPC while the coordination plane sits in another.
- The coordination bucket holds tiny objects (a few hundred bytes per VM). Capacity, throughput, and cost are negligible; even a free-tier or single-container MinIO works.
- When the same bucket is used for both targets, separate prefixes (e.g. `harvester/...` for data, `dr/...` for coordination) keep the namespaces apart.

### 2.2 Coordination plane

Ownership of each protected VM is recorded in a per-VM JSON object on the **DR coordination target** at a deterministic key:

```
s3://<coordination-bucket>/dr/vms/<namespace>/<vmName>/owner.json
```

This object holds the active cluster identity, a lease, and the latest ready backup pointer. Updates are serialized via S3 conditional writes (`If-None-Match: *` for create, `If-Match: <etag>` for update) so two clusters cannot both promote themselves to primary.

Example `owner.json`:

```json
{
  "apiVersion": "dr.harvesterhci.io/v1alpha1",
  "kind": "VMOwnership",
  "namespace": "default",
  "vmName": "vm1",
  "activeCluster": "cluster-a",
  "standbyCluster": "cluster-b",
  "phase": "PrimaryActive",
  "generation": 1,
  "holderIdentity": "cluster-a/harvester-dr-controller",
  "leaseID": "cluster-a-lease-0001",
  "renewTime": "2026-05-03T10:00:00Z",
  "leaseDurationSeconds": 180,
  "latestReadyVMBackup": null,
  "latestReadyVMBackupTime": null
}
```

### 2.3 CRDs

Two cluster-scoped behaviors are introduced via CRDs:

**`DRProtectedVM`** — declares that a VM participates in DR. Applied symmetrically on both clusters. Each side carries its own `localClusterID` and the shared `initialPrimaryCluster` value:

```yaml
apiVersion: dr.harvesterhci.io/v1alpha1
kind: DRProtectedVM
metadata:
  name: vm1
  namespace: default
spec:
  vmName: vm1
  localClusterID: cluster-a
  peerClusterID: cluster-b
  initialPrimaryCluster: cluster-a
  s3Coordination:
    bucket: harvester-dr-control
    key: dr/vms/default/vm1/owner.json
    lockMode: ConditionalWrite
  backupPolicy:
    schedule: "*/30 * * * *"
    retention: 24
    scheduleVMBackupName: vm1-dr-schedule
  failoverPolicy:
    mode: Manual
    requireFencing: true
    allowAutoFailover: false
```

`DRProtectedVM` describes *participation in DR*, not *current primary status*. Current primary is determined by `owner.json`, not by the CR.

**`DRFailoverRequest`** — operator-issued request to transition ownership. Carries the target cluster and, for unplanned failover, an explicit fencing confirmation:

```yaml
apiVersion: dr.harvesterhci.io/v1alpha1
kind: DRFailoverRequest
metadata:
  name: failover-vm1-to-cluster-b
  namespace: default
spec:
  protectedVMRef:
    name: vm1
  targetCluster: cluster-b
  mode: Planned          # or Unplanned
  startVM: true
  fencingConfirmation:   # required when mode = Unplanned
    method: Manual
    message: "I confirm cluster-a is fenced or vm1 cannot run on cluster-a."
```

### 2.4 Roles per cluster

| Role | Allowed actions |
|------|-----------------|
| Primary (`activeCluster == localClusterID`) | Run VM; own active `ScheduleVMBackup`; renew lease; publish latest ready `VMBackup` into `owner.json` |
| Secondary | Ensure local VM is not running; no active schedule; watch synced `VMBackup` metadata; mark `StandbyReady` when restorable |

### 2.5 Reuse of Harvester primitives

The DR controller does not invent a separate data path. It coordinates ownership and drives existing Harvester resources:

- **`ScheduleVMBackup`** — periodic backups, owned by the primary only.
- **`VMBackup`** — backup instances; metadata is discovered on the secondary via the existing `MetadataHandler.syncVMBackup` path at [pkg/controller/master/backup/backup_metadata.go:366](../pkg/controller/master/backup/backup_metadata.go#L366), which lists the shared backup target and creates local `VMBackup` CRs from the JSON metadata.
- **`VMRestore`** — created on the secondary at failover time to materialize the VM from the latest ready backup.

The controller only needs:
- Local Kubernetes API access.
- Read access to the configured backup target (so it can observe `VMBackup` metadata sync).
- Read/write access to the configured DR coordination target (independently configured; may carry separate credentials).
- Permission to manage local `VMBackup` / `ScheduleVMBackup` / `VMRestore` CRs.
- Permission to read local VM state.

No remote-cluster kubeconfig is required.

---

## 3. Workflows

### 3.1 Bootstrap

1. Operator configures the same backup target on both clusters.
2. Operator configures the same DR coordination target on both clusters. This is an S3 (or MinIO) bucket with conditional-write support; it may share storage with the backup target or sit on a separate backend.
3. Operator verifies the coordination target with `cwprobe` (or equivalent) before installing the controller — the probe must pass on both clusters, otherwise DR cannot operate safely (see §5.1).
4. Operator installs `harvester-dr-controller` on both clusters.
5. Operator applies `DRProtectedVM` for the same logical VM on both clusters, agreeing on `initialPrimaryCluster`.
6. Controller on `cluster-a` (the initial primary):
   - Reads `DRProtectedVM`, sees `localClusterID == initialPrimaryCluster`.
   - Creates `owner.json` with `If-None-Match: *`. If it already exists, treats existing content as truth.
   - Sets local role to Primary.
   - Ensures the VM is allowed to run.
   - Creates/enables `ScheduleVMBackup`.
7. Controller on `cluster-b` (the initial secondary):
   - Reads `DRProtectedVM`, sees `localClusterID != initialPrimaryCluster`.
   - Reads `owner.json`. Does not create it.
   - Sets local role to Secondary.
   - Ensures the local VM is not running.
   - Does not create `ScheduleVMBackup`.
   - Watches synced `VMBackup` metadata. Marks `StandbyReady` when the latest ready backup is fully restorable.

### 3.2 Steady state

**Primary loop:**
1. Read `owner.json`. If `activeCluster != localClusterID`, demote and stop acting as primary.
2. Renew the lease via conditional write.
3. Ensure `ScheduleVMBackup` is present and enabled.
4. Watch `VMBackup` objects. Identify the latest with `Ready == True`.
5. Update `owner.json` with the latest ready backup name and timestamp.
6. Update `DRProtectedVM` status.

**Secondary loop:**
1. Read `owner.json`. Confirm `activeCluster != localClusterID`.
2. Ensure `ScheduleVMBackup` is absent or disabled locally.
3. Ensure the local VM is not running.
4. Verify the latest ready backup referenced in `owner.json` is visible and restorable locally.
5. Update `DRProtectedVM` status with `StandbyReady` and observed backup pointer.

### 3.3 Planned failover

Triggered by a `DRFailoverRequest` with `mode: Planned`.

1. Receiving controller writes `phase: PlannedFailoverRequested` and `requestedTargetCluster` into `owner.json` via conditional write.
2. Source-side controller observes the request:
   - Stops the VM.
   - Disables `ScheduleVMBackup`.
   - Triggers a final `VMBackup` and waits for `Ready == True`.
   - Updates `owner.json` to `phase: FailoverPrepared`, sets `sourceVMStopped = true`, updates `latestReadyVMBackup`.
3. Target-side controller observes prepared state:
   - Verifies the final backup is visible locally.
   - Conditionally swaps `activeCluster` and `standbyCluster`, sets `phase: FailingOver`.
   - Creates `VMRestore` from the final backup.
   - Starts the VM (if `startVM: true`).
   - Creates/enables `ScheduleVMBackup`.
   - Sets `phase: PrimaryActive`.

### 3.4 Unplanned failover

Triggered by a `DRFailoverRequest` with `mode: Unplanned` and `fencingConfirmation`.

1. Secondary controller observes that the primary's lease has expired but does **not** auto-promote. Surfaces conditions:
   - `PrimaryLeaseExpired = True`
   - `FailoverPossible = True`
   - `FencingRequired = True`
2. Operator confirms fencing externally and applies a `DRFailoverRequest` with `fencingConfirmation`.
3. Controller validates: lease expired, fencing confirmation present, latest ready backup visible.
4. Controller conditionally writes ownership transfer into `owner.json`, recording `previousActiveCluster` and `transitionReason`.
5. Controller creates `VMRestore`, starts the VM, enables `ScheduleVMBackup`.

### 3.5 Old-primary recovery

When a previously-active cluster comes back online after an unplanned failover:

1. Controller reads `owner.json`, observes `activeCluster != localClusterID`.
2. Demotes itself to Secondary.
3. Disables any local `ScheduleVMBackup`.
4. If the local VM is still running, stops it with grace period (default behavior; opt-out via annotation).
5. Awaits an explicit failback request. No auto-failback.

### 3.6 Failback

Failback is the planned-failover workflow in the opposite direction. There is no special "failback" code path.

---

## 4. Invariants

These rules are enforced by the controller in every reconcile:

1. Only the cluster named in `owner.json` as `activeCluster` may run the VM.
2. Only the `activeCluster` may have an active `ScheduleVMBackup` for the VM.
3. The secondary may read backup metadata and prepare for restore but must not start the VM.
4. All ownership changes use conditional writes against `owner.json`. No blind overwrites.
5. Planned failover requires the source VM stopped and a final backup `Ready` before the target may take ownership.
6. Unplanned failover requires lease expiry observed by the secondary plus an explicit fencing confirmation in the `DRFailoverRequest`.
7. On recovery, the old primary must read `owner.json` before resuming any VM-affecting action.
8. Failback is never automatic.

---

## 5. Design considerations

### 5.1 S3 conditional writes

The ownership model rests on `If-Match` / `If-None-Match` semantics on `PutObject`. Without compare-and-swap on the shared `owner.json`, two clusters can promote themselves simultaneously — the exact split-brain scenario this design exists to prevent.

Backend support varies:

| Backend | Conditional PUT support |
|---------|-------------------------|
| AWS S3 | Yes (added Nov 2024) |
| MinIO | Yes |
| Cloudian HyperStore | Partial / version-dependent |
| Wasabi | Partial / version-dependent |
| NetApp StorageGRID | Partial / version-dependent |
| Older on-prem appliances | Often no |

S3 Object Lock is a different primitive (WORM retention, not compare-and-swap) and does not solve the acquisition race on its own. NFS targets cannot do conditional writes at all — which is one of the reasons the architecture in §2.1 separates the coordination target from the backup target. Operators with NFS backup targets can still use DR; they just point the controller at any S3-with-conditional-writes bucket as the coordination target.

**Decision:** require conditional-write support on the coordination target and probe at startup.

- The DR controller probes the configured **DR coordination target** at startup by writing a sentinel object twice with `If-None-Match: *`. The probe succeeds only if the second write returns `412 Precondition Failed`. The same probe is exposed as the standalone `cwprobe` utility (see §5.12) so operators can verify a backend before installing the controller.
- If the probe fails, the controller refuses to start and surfaces a clear error: "configured coordination target does not support conditional writes; DR cannot operate safely on this backend."
- Failure is loud and early (controller refuses to start) rather than silent and late (split-brain at failover time).
- A supported-backend matrix is published in release notes. AWS S3 (since Nov 2024) and MinIO 2024-Q4 or newer cover the bulk of real Harvester deployments.

If broader backend support is needed in a later release, the same probe gate can be reused as a feature flag — controllers on unsupported backends remain inert, while a separate coordination plane (e.g., Rancher when present) is layered in.

### 5.2 RPO and backup cadence

`ScheduleVMBackup` is the mechanism that controls backup creation frequency, and its cron granularity is flexible — operators can run backups as frequently as their environment tolerates. Snapshot and backup retention is handled by `ScheduleVMBackup` itself: outdated Longhorn snapshots and backups are pruned automatically, so per-volume snapshot accumulation is not a concern at the design level.

The practical RPO floor is set by the cost of each backup cycle, not by what the cron field accepts:

- Each `VMBackup` triggers a Longhorn volume snapshot plus an incremental upload to S3. IO impact on the VM, S3 bandwidth, and per-cycle CPU all scale with cadence.
- If a backup takes longer than the cron interval, runs queue or skip — cranking the schedule below the actual backup duration buys nothing.
- RTO at the secondary scales with how much delta must be replayed during restore.

The HEP should publish recommended cadence ranges tied to typical VM size and S3 throughput so operators can choose values with their eyes open.

### 5.3 VM dependencies

`VMBackup` already captures the boot-critical secrets via `getSecretBackups` at [pkg/controller/master/backup/backup.go:437](../pkg/controller/master/backup/backup.go#L437), bundling the following into `VMBackup.Status.SecretBackups`:

- SSH public-key secrets (from `AccessCredentials`)
- User-password secrets
- Cloud-init `UserDataSecretRef`
- Cloud-init `NetworkDataSecretRef`

These flow through backup metadata to the secondary, so secrets required for boot are not a gap.

`NetworkAttachmentDefinition`s are out of scope for this design. The operator pre-provisions matching NADs and the underlying VLAN configuration on the secondary cluster as part of DR setup, the same way they pre-configure the shared backup target. The HEP should call this out as a setup prerequisite. Optionally, the secondary controller may emit a `MissingNAD` warning when a referenced NAD is absent at the time `DRProtectedVM` is applied.

### 5.4 Network identity

A failed-over VM acquires a new IP/MAC unless the L2 segment and any VIPs are also failed over. The following are explicitly **out of scope** for this design:

- Application-level reconnection.
- DNS cutover (and the associated TTL caveats).
- VIP / external load-balancer reconfiguration.
- Storage-network reachability between the secondary and existing volume consumers.

These belong in a separate "DR networking" enhancement.

### 5.5 Lease semantics

The secondary must never trust a wall-clock value written by the primary for its own timing decisions. Wall-clock comparison across two clusters introduces clock-skew false positives — the secondary may declare the lease expired while the primary is healthy, or fail to detect a real outage.

The lease object stores a duration, not a deadline:

```json
{
  "renewTime": "2026-05-03T10:00:00Z",
  "leaseDurationSeconds": 180
}
```

`renewTime` is informational only (for humans and debugging). It does not drive the secondary's expiry logic.

The secondary's expiry algorithm:

1. Primary writes `leaseDurationSeconds` (a duration) and `renewTime` (informational).
2. Secondary reads `owner.json` and records `localObservedAt = mySecondary.clock.now()` along with the object's etag.
3. On each subsequent read, if the etag has changed (the primary renewed), the secondary updates `localObservedAt` to its current local clock.
4. If `mySecondary.clock.now() - localObservedAt > leaseDurationSeconds`, the secondary treats the lease as expired.

The only clock involved in the expiry decision is the secondary's own clock, measuring its own elapsed time. Clock skew between clusters becomes irrelevant.

### 5.6 `DRFailoverRequest` placement

The `DRFailoverRequest` CR is an intent declaration; it carries `targetCluster` to say which cluster should become the new primary. Where the operator applies it is a convenience question. The controllers route work to the right side based on the CR contents and the current state in `owner.json`, not based on which cluster received the apply.

Mechanics:

1. Operator applies the CR on whichever cluster is convenient.
2. The receiving controller mirrors the request intent into `owner.json` (e.g., sets `phase: PlannedFailoverRequested`, `requestedTargetCluster: cluster-b`).
3. Both controllers observe the change via their normal S3 polling.
4. Each controller decides if it has work:
   - If `targetCluster == localClusterID`: I'm becoming the new primary. Do target-side work (wait for source-prepared, acquire ownership, create `VMRestore`, start VM, enable schedule).
   - If `activeCluster == localClusterID` and `mode == Planned`: I'm the current primary being asked to hand off. Do source-side work (stop VM, disable schedule, take final backup, mark `FailoverPrepared`).
   - Otherwise: nothing to do; just update local status to reflect that a failover is in progress.

Concrete cases:

- **Planned failover, operator at source (`cluster-a`)**: applies the CR on `cluster-a` while finishing maintenance work. `cluster-a`'s controller mirrors intent into S3, then immediately starts the source-side work itself. `cluster-b` sees `FailoverPrepared` later and runs the target-side work.
- **Planned failover, operator at target (`cluster-b`)**: applies the CR on `cluster-b`. `cluster-b`'s controller mirrors intent into S3 and waits. `cluster-a` sees the request and starts source-side work. `cluster-b` finishes target-side work after.
- **Unplanned failover, source is down**: in practice the operator can only apply on `cluster-b`, since the source's API is unreachable. The CR shape is the same — this is a deployment reality, not an API constraint.

A subtle benefit: this design makes the case where the operator applies on the "wrong" cluster (e.g., on the source when they meant target) recoverable — the CR's `targetCluster` field is what matters, not where the YAML landed.

### 5.7 Restart resilience

Ownership transitions move through phases: `PrimaryActive`, `PlannedFailoverRequested`, `FailoverPrepared`, `FailingOver`, `PrimaryActive`. Each phase must:

- Have an idempotent action (re-running it after a controller restart causes no harm).
- Have a deterministic advance condition (the next phase is entered only when an externally-observable predicate holds).
- Have a rollback or abort path (failed transitions must not strand ownership in an unrecoverable phase).

The HEP should include a state-machine diagram capturing all phases and their transitions.

### 5.8 Polling cost

Per-VM polling of `owner.json` scales linearly with the number of protected VMs and the number of clusters. For environments with hundreds of protected VMs this becomes a meaningful S3 GET load. Mitigation options:

- Aggregate ownership for multiple VMs into a single object via a `DRProtectionGroup` CRD.
- Maintain a per-cluster epoch object that summarizes whether anything changed since the last poll, allowing controllers to skip detailed reads.
- Use bucket notifications (SNS / MinIO webhooks) where supported, falling back to polling otherwise.

### 5.9 Standby readiness

A `VMBackup` CR existing locally on the secondary does not by itself prove the underlying Longhorn volume backup blocks are complete in the backup store — the metadata handler creates the CR from a JSON descriptor; block completeness is a separate signal.

`StandbyReady = True` must require that the referenced backup is verifiably restorable. Otherwise operators discover at failover time that the most recent backup is half-uploaded.

### 5.10 Old-primary split-brain handling

**Scenario.** After an unplanned failover, the original primary may eventually recover:

1. Initial state: `cluster-a` is primary, VM running. `cluster-b` is secondary.
2. `cluster-a` becomes unreachable — network partition, power loss, controller crash, anything. Critically, the VM may still be running on `cluster-a` if the cause was network-only; Harvester locally has no signal that anything is wrong.
3. Operator confirms fencing externally and triggers unplanned failover. `cluster-b` restores from the latest backup and becomes the new primary. `owner.json` records `activeCluster: cluster-b`.
4. Sometime later, `cluster-a` recovers — network restored, power back, controller restarted.

At step 4, `cluster-a` wakes up with stale local state: it still believes it is primary, the VM may still be running locally with disk writes accumulating, and the same VM is also running on `cluster-b` with its own divergent writes. This is split-brain — same logical VM, two running instances, two diverged disk states. The longer it goes unnoticed, the worse the cleanup.

**Required action.** When the recovered controller reads `owner.json` and observes `activeCluster != localClusterID`, it must take action. Two options:

- **Soft:** emit an Event such as `SplitBrainSuspected` and wait for a human. Problem: by the time anyone notices, the local VM has been writing divergent state for hours, making the situation worse.
- **Hard:** stop the local VM with a grace period. Bounds the damage immediately.

The default is **hard**. The safe assumption when in doubt is "the cluster that won the failover is authoritative." Operators who want manual control can opt in; the unsophisticated case must not silently accumulate hours of divergent writes.

**Escape hatch.** An annotation on `DRProtectedVM` (e.g., `dr.harvesterhci.io/manual-split-brain-resolution=true`) reverts behavior on that VM to event-only. Operators set it ahead of time when they want to perform manual data merge, forensic capture, or other rescue operations on the old primary's disks.

**Trade-off.** Auto-stop means that if the operator was in the middle of rescuing data from the old primary, the controller may pull the rug. The annotation handles this case, but it must be set *before* the old primary's controller wakes up and reads `owner.json` — otherwise the auto-stop will already have fired by the time the operator can react.

### 5.11 `DRFailoverRequest` lifecycle

The CRD needs explicit handling for:

- **TTL / GC** of completed requests.
- **Conflicting requests** — what happens if two are applied targeting different clusters concurrently.
- **Webhook validation** — reject requests where `targetCluster` already equals `activeCluster` per `owner.json`, or where `fencingConfirmation` is missing for `mode: Unplanned`.

### 5.12 Coordination plane: the in-tree `pkg/coordstore` package

The compare-and-swap primitive needed for `owner.json` arbitration lives as a native Harvester package at [pkg/coordstore/](../pkg/coordstore/), with an S3 implementation under [pkg/coordstore/s3/](../pkg/coordstore/s3/). An earlier iteration of this work was prototyped as an extension to the Longhorn `backupstore` package; the upstream PR was not pursued, and the implementation is in-tree.

Detailed design notes for the package — interface shape, why it is independent of `backupstore`, S3 implementation details, the operator-facing `cwprobe` utility, and the consumer pattern the DR controller should follow — live in [study/coordstore-design.md](coordstore-design.md). Summary:

**Public API.** [pkg/coordstore/store.go](../pkg/coordstore/store.go) defines a backend-agnostic interface plus two sentinel errors:

```go
type Store interface {
    Read(ctx context.Context, key string) (data []byte, version string, err error)
    Create(ctx context.Context, key string, data []byte) (version string, err error)
    Update(ctx context.Context, key string, data []byte, expectedVersion string) (newVersion string, err error)
    Delete(ctx context.Context, key string) error
    Probe(ctx context.Context, probePrefix string) error
}

var ErrPreconditionFailed = errors.New("coordstore: conditional write precondition failed")
var ErrNotFound           = errors.New("coordstore: object not found")
```

`version` is opaque (the S3 implementation uses ETags). The DR controller reads `owner.json` to obtain the current version, and updates it with `Update(ctx, key, newData, version)`; concurrent updates lose the race with `ErrPreconditionFailed` and re-read.

**S3 implementation.** [pkg/coordstore/s3/s3.go](../pkg/coordstore/s3/s3.go) uses Harvester's already-vendored `aws-sdk-go` v1. Because v1's `PutObjectInput` does not expose the `IfMatch`/`IfNoneMatch` fields AWS added in November 2024, the implementation builds the request via `PutObjectRequest`, sets the conditional header on the raw HTTP request, and calls `Send()`. The header is included in the SigV4 canonical request, so signing is correct.

`Probe` writes a uniquely-named sentinel under the caller-supplied prefix twice with `If-None-Match: *` and verifies the second write is rejected with `ErrPreconditionFailed`. The sentinel is removed in a deferred cleanup. This is the same probe semantics referenced by §5.1.

**Operator tool.** [cmd/cwprobe/main.go](../cmd/cwprobe/main.go) is a CLI that runs every CAS check in sequence (probe, missing-read, create-rejects-existing, update-with-current-version, fresh-version-returned, update-with-stale-version-rejected, read-after-CAS) and reports per-check `[PASS]`/`[FAIL]` lines. Exit `0` = safe, `1` = at least one check failed (backend NOT safe), `2` = setup error. Used as the pre-deployment verification step in §3.1.

**Why in-tree, not as a backupstore extension.**

- The DR controller does not need backupstore's full driver model just to write a small JSON object.
- `coordstore.Store` is backend-agnostic; future implementations (etcd, NFSv4 byte-range locks, Rancher) drop in as sibling packages without consumer changes.
- Backupstore's `BackupStoreDriver` interface is untouched, so nothing in the existing backup-data path can regress as a result of this work.

---

## 6. Open questions

- [ ] Which S3 backends are officially supported for the coordination plane? Same set as for backup target, or a stricter subset?
- [ ] Should the `DRProtectedVM` CR carry the coordination-target reference inline, or should the coordination target be cluster-scoped configuration (e.g., a `DRCoordinationTarget` CR or a Harvester setting) so it isn't repeated per VM?
- [ ] What is the recommended RPO range, with concrete cadence guidance tied to VM size and S3 throughput?
- [ ] Should Rancher (when present) become the coordination plane instead of S3, sidestepping the conditional-write compatibility problem entirely for Rancher-managed Harvester clusters?
- [ ] Does the secondary need the originating `VirtualMachineImage` CR for any restore path (e.g., display-name lookup, future re-clone), or is volume data alone sufficient?
- [ ] `DRProtectedVM` per-VM vs. a `DRProtectionGroup` selecting VMs by label — at what scale does the group abstraction become necessary?
- [ ] HA model for the DR controller itself: single replica with leader election? What is the behavior if the controller is down during a failover transition?
- [ ] Webhook validation for `DRProtectedVM`: reject if both clusters disagree on `initialPrimaryCluster` or `s3Coordination.key`. (Detectable at controller startup, but earlier rejection at apply time is friendlier.)
- [ ] Observability: which metrics and events are exposed? At minimum, lease age, last-backup age, S3 round-trip latency, failover duration, and an event per phase transition.
- [ ] Upgrade path: what happens to in-flight failovers across DR-controller upgrades on either side?

---

## 7. Next steps

1. Decide the coordination plane (S3 conditional writes vs Rancher vs another mechanism). Gates everything else.
2. Publish RPO guidance covering cadence vs. VM size vs. S3 throughput vs. Longhorn snapshot budget.
3. Document the NAD pre-provisioning prerequisite as part of DR setup.
4. Draft a HEP under [enhancements/](../enhancements/) modeled on [20220413-zero-downtime-upgrade.md](../enhancements/20220413-zero-downtime-upgrade.md), with sections covering:
   - Goals and non-goals (network identity called out explicitly).
   - RPO / RTO statement.
   - Backend compatibility matrix.
   - State-machine diagram for ownership transitions.
   - CRD reference for `DRProtectedVM` and `DRFailoverRequest` (and `DRProtectionGroup` if adopted).
   - Failure modes and recovery procedures.
   - Upgrade path for in-flight failovers.
