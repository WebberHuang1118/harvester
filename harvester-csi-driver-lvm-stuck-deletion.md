# `harvester-csi-driver-lvm` HelmChart stuck in deletion

Investigation of support bundle `supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z`.

## Symptom

The `harvester-csi-driver-lvm` HelmChart in `harvester-system` was deleted but never finished uninstalling. Status keeps reporting `JobCreated=True` / `Failed=False` and `jobName: helm-delete-harvester-csi-driver-lvm`, while the rke2-server log keeps emitting `waiting for delete of helm chart …, requeuing`.

See the HelmChart object: [helmcharts.yaml](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/helm.cattle.io/v1/helmcharts.yaml#L3).

## Root cause (one sentence)

**Proximate cause:** the pre-delete hook Job `delete-harvester-csi-driver-lvm-webhook` from the first `helm uninstall` succeeded but was never cleaned up; every subsequent `helm uninstall` attempt fails at `jobs.batch "delete-harvester-csi-driver-lvm-webhook" already exists`, so the release stays in `uninstalling` and helm-controller loops forever. The deeper root cause — *why* the first helm-delete pod was killed before it could clean up its hook — is not pinned down by this bundle; see "The load-bearing question" section below.

## Evidence

### 1. The smoking gun in the helm log

[`helm-delete-harvester-csi-driver-lvm-sbf48/helm.log`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/logs/harvester-system/helm-delete-harvester-csi-driver-lvm-sbf48/helm.log#L27-L44):

```
+ LINE=harvester-csi-driver-lvm-1.8.1-rc1,uninstalling,1
…
+ helm uninstall harvester-csi-driver-lvm --namespace harvester-system --wait
Error: warning: Hook pre-delete harvester-csi-driver-lvm/templates/uninstall-hook.yaml failed: 1 error occurred:
    * jobs.batch "delete-harvester-csi-driver-lvm-webhook" already exists
+ true
+ exit
```

Two things to notice:

- The release is already in `uninstalling` state, meaning an earlier `helm uninstall` got partway through and didn't finish.
- The current attempt aborts at the very first step — creating the pre-delete hook Job — because a leftover one is still there.

### 2. The leftover hook Job

[`jobs.yaml` (`delete-harvester-csi-driver-lvm-webhook`)](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L157-L300):

- `creationTimestamp: 2026-06-12T23:09:13Z`
- `completionTime: 2026-06-12T23:09:19Z` → hook succeeded in 6 s
- `succeeded: 1`
- **No `deletionTimestamp`** → Helm never even tried to delete it
- Finalizer: `wrangler.cattle.io/promote-node-controller`

With `helm.sh/hook-delete-policy: hook-succeeded` (the policy in the chart), Helm is supposed to delete this Job right after the hook succeeds. It never did — the first `helm-delete-…` pod exited before reaching that cleanup step, leaving the Job in place.

### 3. The hook is defined here

[`charts/charts/harvester-csi-driver-lvm/templates/uninstall-hook.yaml`](charts/charts/harvester-csi-driver-lvm/templates/uninstall-hook.yaml#L1-L24):

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: delete-harvester-csi-driver-lvm-webhook
  annotations:
    "helm.sh/hook": pre-delete
    "helm.sh/hook-delete-policy": hook-succeeded
```

### 4. The retry loop in rke2-server

[`rke2-server.log`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3743-L3783):

```
23:03:14  Applying HelmChart from harvester-csi-driver-lvm … using Job harvester-system/helm-install-harvester-csi-driver-lvm
23:09:14  handler on-helm-chart-remove: waiting for delete of helm chart for harvester-system/harvester-csi-driver-lvm by helm-delete-harvester-csi-driver-lvm, requeuing
23:09:14  handler on-helm-chart-remove: DesiredSet - Replace Wait batch/v1, Kind=Job harvester-system/helm-delete-harvester-csi-driver-lvm for helm-chart-registration harvester-system/harvester-csi-driver-lvm, requeuing
23:09:17  …same two lines repeating…
23:09:48  …
23:10:15  …
23:12:18  …
23:14:15  …
```

The retries are driven by helm-controller's HelmChart `OnRemove` handler.

### 5. The parent helm-delete Job has been recreated (once)

[`jobs.yaml` (`helm-delete-harvester-csi-driver-lvm`)](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L914):

- `creationTimestamp: 2026-06-12T23:12:15Z` — far later than the first helm-delete pod `…-b5hxg` at 23:09:11.
- Pod `…-sbf48` is the only one of four pods (`b5hxg`, `45qjv`, `tb78f`, `sbf48`) still present in [`pods.yaml`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/v1/pods.yaml#L15882).

Reading [`events.yaml`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/v1/events.yaml) for the Job shows the lifecycle:

| Time | Event | Notes |
|---|---|---|
| 23:09:11 | `SuccessfulCreate` (`…-b5hxg`) | First helm-delete pod |
| 23:09:14 | `SuccessfulCreate` (`…-45qjv`) | Second helm-delete pod |
| 23:09:45 | `SuccessfulCreate` (`…-tb78f`) | Third helm-delete pod |
| **23:09:50** | **`Completed`** | **Some helm-delete pod exited 0 → Job marked complete** |
| **23:12:15** | **`SuccessfulCreate` (`…-sbf48`)** | **New helm-delete Job (creationTimestamp 23:12:15) creates its first pod** |
| 23:12:20 | `Completed` | The new Job's pod exited 0 |

Two things the bundle **cannot** distinguish, because only the latest Job + its surviving pod `sbf48` are preserved (the other three pods' `controller-uid` labels are gone from the bundle):

1. Whether `b5hxg`, `45qjv`, `tb78f` are **`backoffLimit` retries of one Job** (`podReplacementPolicy: TerminatingOrFailed` allows a replacement pod as soon as the previous one is `Terminating`) — or whether the parent Job was itself replaced two or three times during that 39 s window.
2. Whether wrangler issued any `apply.ErrReplace` decisions on the helm-delete Job between 23:09:11 and 23:09:50.

What's certain is that **a parent-Job recreation definitely happened at least once**, between 23:09:50 (first Job's `Completed` event) and 23:12:15 (new Job's `creationTimestamp`). The mechanism behind that recreation is covered in the next section.

## The two controllers in play

Both have "remove" in the name and both touch the helm-delete Job — easy to confuse.

| | helm-controller's `on-helm-chart-remove` | Harvester's `promote-node-controller` |
|---|---|---|
| Source | [`helm-controller/pkg/controllers/chart/chart.go`](helm-controller/pkg/controllers/chart/chart.go#L186) | [`pkg/controller/master/node/promote_controller.go`](pkg/controller/master/node/promote_controller.go#L95) |
| Watches | HelmChart | Job |
| Registration | `remove.RegisterScopedOnRemoveHandler(ctx, helms, "on-helm-chart-remove", …)` | `jobs.OnRemove(ctx, promoteControllerName, promoteController.OnJobRemove)` |
| Finalizer it adds | `wrangler.cattle.io/on-helm-chart-remove` (HelmChart) | `wrangler.cattle.io/promote-node-controller` (every Job in ns) |
| Per pass | Builds helm-delete Job, [`apply.WithSetID("helm-chart-registration").ApplyObjects(...)`](helm-controller/pkg/controllers/chart/chart.go#L394), [`time.Sleep(jobSettleTime)`](helm-controller/pkg/controllers/chart/chart.go#L416), then [`waiting for delete of helm chart for %s by %s`](helm-controller/pkg/controllers/chart/chart.go#L439) until the Job succeeds — **requeues every time** | [`OnJobRemove`](pkg/controller/master/node/promote_controller.go#L173-L199): if Job lacks `HarvesterPromoteNodeLabelKey`, returns `nil` so wrangler clears the finalizer |
| Role | **Active driver** of the recreate loop | **Passive bystander** — adds wall-clock between Delete and actual removal, nothing more |

Helm-controller's [`reconcileJob`](helm-controller/pkg/controllers/chart/chart.go#L209-L224) (called from the apply layer registered at [chart.go:164](helm-controller/pkg/controllers/chart/chart.go#L164)):

```go
func (c *Controller) reconcileJob(oldObj, newObj runtime.Object) (bool, error) {
    oldJob, _ := objectToJob(oldObj)
    newJob, _ := objectToJob(newObj)
    if templateChanged(oldJob, newJob) {
        minAge := metav1.Now().Add(-jobSettleTime)  // jobSettleTime = 3s
        if cacheJob, err := c.jobCache.Get(oldJob.Namespace, oldJob.Name);
           err == nil && cacheJob.CreationTimestamp.After(minAge) {
            return false, errors.New("wait for Job to settle before replacing")
        }
        return false, apply.ErrReplace
    }
    return false, nil
}
```

[`templateChanged`](helm-controller/pkg/controllers/chart/chart.go#L1345-L1349) only fires when wrangler's apply finds two objects with the same Kind+Name in the cached set vs the desired set. The install Job (`helm-install-…`) and delete Job (`helm-delete-…`) have **different names** (see [chart.go#L640-L664](helm-controller/pkg/controllers/chart/chart.go#L640-L664)), so `templateChanged` is never invoked for the install→delete swap — that swap is performed by wrangler's SetID-based set-difference, not by a template comparison. The `"DesiredSet - Replace Wait"` lines at [`rke2-server.log#L3760`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3760), [`#L3762`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3762), [`#L3767`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3767), [`#L3783`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3783) come from a different situation: wrangler trying to (re)create the helm-delete Job while a same-named previous instance is still `Terminating` because of the `wrangler.cattle.io/promote-node-controller` finalizer. See the next subsection for the full mechanism.

### How the install→delete swap actually works (it's *not* `templateChanged`)

The install and delete Jobs have **different names** — see [chart.go#L640-L664](helm-controller/pkg/controllers/chart/chart.go#L640-L664):

```go
action := "install"
if chart.DeletionTimestamp != nil {
    action = "delete"
}
…
Name: fmt.Sprintf("helm-%s-%s", action, chart.Name),
```

So:

- Install Job: `helm-install-harvester-csi-driver-lvm` ([jobs.yaml#L1547](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L1547))
- Delete Job: `helm-delete-harvester-csi-driver-lvm` ([jobs.yaml#L914](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L914))

`reconcileJob` / `templateChanged` only fires when wrangler's apply finds two objects with the **same** `Kind` and `Name` in the desired set vs the cached set. Different names → no template comparison, no `apply.ErrReplace`, no `templateChanged` decision. **In our scenario `templateChanged` never returns true at all.**

What actually performs the swap is wrangler apply's **SetID-based set-difference logic**. Both the install and delete applies use [`WithSetID("helm-chart-registration")`](helm-controller/pkg/controllers/chart/chart.go#L394), and wrangler records set membership per SetID + owner. When the desired set changes:

| Before delete apply (after install) | After delete apply |
|---|---|
| `{ helm-install-… Job, ServiceAccount, ClusterRoleBinding, ConfigMap, Secret, … }` | `{ helm-delete-… Job, ServiceAccount, ClusterRoleBinding, ConfigMap, Secret, … }` |

Wrangler walks the diff and does:

1. **Delete** any object in the old set but not the new set → the install Job is removed because it's no longer a member of the set under SetID `helm-chart-registration`.
2. **Create** any object in the new set but not the old set → the delete Job is created.
3. **For objects present in both** (same Kind+Name — e.g. the ServiceAccount, ClusterRoleBinding, ConfigMap, related Secret) wrangler calls the registered patcher / `reconcileJob` to decide whether to patch. None of those overlaps is a Job, so `reconcileJob` is never even called during the install→delete transition.

`templateChanged` is only relevant for the case where you re-apply a chart whose install Job already exists (e.g. you change the chart values, helm-controller renders a new install Job with the same name as the existing one, and `reconcileJob` detects the template difference and returns `apply.ErrReplace` so wrangler deletes & recreates the install Job). That code path doesn't run for our HelmChart at all.

### What the `"DesiredSet - Replace Wait"` lines actually mean

Given the above, the `"DesiredSet - Replace Wait …"` lines for the helm-delete Job at [`rke2-server.log#L3760-L3783`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3760-L3783) are **not** from a `templateChanged → ErrReplace` decision. They're emitted by wrangler when it wants to place an object into the cluster but a previous version with the same name is still `Terminating` (has `deletionTimestamp` but the API server hasn't actually removed it yet). The most likely trigger is the recreate cycle:

- `OnRemove`'s success-path empty apply at [chart.go#L446-L451](helm-controller/pkg/controllers/chart/chart.go#L446-L451) deletes the helm-delete Job. It goes `Terminating` because of the `wrangler.cattle.io/promote-node-controller` finalizer.
- A subsequent `OnRemove` pass runs the regular apply at [chart.go#L390-L395](helm-controller/pkg/controllers/chart/chart.go#L390-L395), which puts the helm-delete Job back in the desired set. Wrangler sees the cluster still has a `helm-delete-…` Job with a `deletionTimestamp` → emits Replace Wait and re-queues.
- Once `OnJobRemove` clears the finalizer and the Job is fully gone, the next apply creates the fresh one (the 23:12:15 creation).

### Why the install Job doesn't have this problem

Two things keep the install Job stable:

1. The install path is registered via [`RegisterHelmChartGeneratingHandler`](helm-controller/pkg/controllers/chart/chart.go#L182-L184) — a *generating handler* only fires `OnChange` when the HelmChart spec changes. The HelmChart's spec didn't change between 23:03:14 and 23:09:11, so `OnChange` ran once, applied the install Job once, and never re-applied.
2. Even if `OnChange` did fire on an unrelated event, the short-circuit at [chart.go#L551-L553](helm-controller/pkg/controllers/chart/chart.go#L551-L553) returns `ErrSkip` when the cached install Job already exists and its template hasn't changed, so apply isn't even invoked.

By contrast, `OnRemove` deliberately returns the [`waiting for delete of helm chart …`](helm-controller/pkg/controllers/chart/chart.go#L439) error on every pass while the helm-delete Job hasn't succeeded ([chart.go#L431-L440](helm-controller/pkg/controllers/chart/chart.go#L431-L440)):

```go
if job.Status.Succeeded <= 0 {
    …
    return newChart, fmt.Errorf("waiting for delete of helm chart for %s by %s", key, job.Name)
}
```

That's what re-queues `OnRemove` indefinitely. Once the orphan hook Job exists (from step 3-4 of "The complete picture"), every subsequent helm-delete pod that survives long enough to reach `helm uninstall` hits `already exists` and exits 0 via the klipper-helm script's trailing `… || true` (see [helm.log#L43-L44](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/logs/harvester-system/helm-delete-harvester-csi-driver-lvm-sbf48/helm.log#L43-L44)). That false-positive success makes `OnRemove` reach the success path, run the empty cleanup apply that tears down the helm-delete Job, and the next requeue recreates it. **`templateChanged` plays no role in this loop** — the swap happened once via set-difference, and from there on it's the success-path teardown + requeue-recreate cycle.

## The complete picture

1. **23:03:14** — HelmChart created; install Job runs and succeeds at 23:03:22.
2. **23:09:11** — HelmChart gets a `deletionTimestamp`. `OnRemove`'s apply at [chart.go#L390-L395](helm-controller/pkg/controllers/chart/chart.go#L390-L395) swaps the install Job for the helm-delete Job. The swap is done by wrangler's SetID-based set-difference (the install Job is no longer a member of the desired set under `helm-chart-registration`, so wrangler deletes it; the helm-delete Job is a new member, so wrangler creates it). Different names → `templateChanged` is **not** involved.
3. **23:09:11–23:09:13** — First helm-delete pod (`b5hxg`) runs `helm uninstall --wait`. It sets the release to `uninstalling` and creates the pre-delete hook Job `delete-harvester-csi-driver-lvm-webhook` ([events.yaml#L12525-L12780](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/v1/events.yaml#L12525-L12780)). Helm then blocks, waiting for the hook to finish.
4. **23:09:15** — kubelet kills `b5hxg` ("Stopping container helm", [events.yaml#L16908-L16954](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/v1/events.yaml#L16908-L16954)) **while it's still blocked on the hook**. The most likely trigger is wrangler/Job-controller deleting the parent Job, which cascades to delete its pods; the exact cause can't be nailed down from the bundle. The hook Job runs to completion on its own at 23:09:19 (separate Job, independent lifecycle), but with `b5hxg` dead **there is no Helm process left to run the `hook-succeeded` cleanup** — so `delete-harvester-csi-driver-lvm-webhook` is left in the cluster permanently.
5. **23:09:14–23:09:45** — Replacement helm-delete pods `45qjv` and then `tb78f` are spawned (either as `backoffLimit` retries within the same parent Job, or as fresh pods under a replaced parent Job — the bundle can't distinguish; see Section 5 above). From this point onward the pre-delete hook Job already exists in the cluster (left over from step 3), so each new pod's `helm uninstall` hits `jobs.batch "delete-harvester-csi-driver-lvm-webhook" already exists`, but the klipper-helm script's trailing `… || true` ([`helm.log#L43-L44`](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/logs/harvester-system/helm-delete-harvester-csi-driver-lvm-sbf48/helm.log#L43-L44)) makes the pod exit 0 anyway. One of them (`tb78f`) survives long enough to exit cleanly, so the parent Job gets marked `Completed` at 23:09:50.
6. **~23:09:50–23:12:15** — `OnRemove` sees `Job.Status.Succeeded > 0` and reaches its success path. The empty apply at [chart.go#L446-L451](helm-controller/pkg/controllers/chart/chart.go#L446-L451), using the same `helm-chart-registration` SetID, tells wrangler "no objects should be in this set" — wrangler deletes the helm-delete Job. The HelmChart still has its `deletionTimestamp` and `on-helm-chart-remove` finalizer, so it's re-queued. The ~2:25 gap before the next pod is wrangler's queue backoff.
7. **23:12:15** — `OnRemove` runs again, finds no helm-delete Job in cache, and recreates one via [chart.go#L390-L395](helm-controller/pkg/controllers/chart/chart.go#L390-L395) — this is the Job in the bundle, `creationTimestamp: 23:12:15`. Pod `sbf48` starts. It also exits 0 via `+ true`, Job marked `Completed` at 23:12:20.
8. The cycle from step 6 will repeat indefinitely, because every helm-delete pod will always exit 0 as long as the leftover hook Job exists. The HelmChart never finalizes.

Note: `templateChanged` never returns true in this scenario at all — the install→delete swap in step 2 is done by wrangler's SetID set-difference (different names), and from step 5 onward `reconcileJob` only ever sees cached `helm-delete-…` vs desired `helm-delete-…`, which are identical. See the "How the install→delete swap actually works" subsection above for the full mechanism.

## Intended vs broken `OnRemove` flow

[`OnRemove`](helm-controller/pkg/controllers/chart/chart.go#L366-L456) is designed as a one-shot terminator: it's meant to return `(chart, nil)` **exactly once** and then never run again. The "never run again" property isn't enforced by OnRemove itself — it's enforced by the `wrangler.cattle.io/on-helm-chart-remove` finalizer machinery that wraps it. Once OnRemove returns nil, the framework removes the finalizer, the HelmChart is fully deleted, and with no HelmChart there are no more events for OnRemove to fire on.

### The intended flow (happy path)

| OnRemove call | What happens |
|---|---|
| **#1** — first fire after `deletionTimestamp` | [L390-L395](helm-controller/pkg/controllers/chart/chart.go#L390-L395) apply: cached set under `helm-chart-registration` had the install Job; desired set has the delete Job. Wrangler set-difference: delete install Job, create helm-delete Job. [L416](helm-controller/pkg/controllers/chart/chart.go#L416) sleep 3 s. [L419](helm-controller/pkg/controllers/chart/chart.go#L419) fetch Job from cache. [L431](helm-controller/pkg/controllers/chart/chart.go#L431): `Succeeded == 0` (pod just started). Update chart status, return `(chart, "waiting for delete of helm chart …")` → requeue. |
| _(meanwhile)_ | helm-delete pod runs `helm uninstall --wait`. Pre-delete hook runs; **Helm cleans up the hook Job per `hook-succeeded`**; Helm deletes main resources; marks release `uninstalled`; container exits 0 → `Job.Status.Succeeded = 1`. |
| **#2** — requeue after Job completes | [L390-L395](helm-controller/pkg/controllers/chart/chart.go#L390-L395) apply: cached helm-delete Job matches desired helm-delete Job → no-op (`templateChanged` false). L416 sleep. L419 fetch Job → `Succeeded == 1`. **L431 branch skipped.** [L443](helm-controller/pkg/controllers/chart/chart.go#L443) emit `RemoveJob` event. [L446-L451](helm-controller/pkg/controllers/chart/chart.go#L446-L451) empty apply: wrangler deletes the helm-delete Job and its sibling resources (ServiceAccount, RBAC, ConfigMap, Secret) under SetID `helm-chart-registration`. [L456](helm-controller/pkg/controllers/chart/chart.go#L456) **`return chart, nil`**. |
| **Framework, after #2 returns nil** | wrangler's `RegisterScopedOnRemoveHandler` sees `nil` error → removes the `on-helm-chart-remove` finalizer from the HelmChart. The HelmChart now has no finalizers left → **the API server actually deletes the HelmChart object**. |
| **No call #3** | OnRemove only fires on HelmChart events. The HelmChart no longer exists → no events → no more OnRemove invocations → **no opportunity to recreate the helm-delete Job**. |

The "uninstall Job is not re-created" property comes from **the HelmChart itself going away**, which removes the source of all further reconciles. There's no special logic in OnRemove to prevent re-creation — it just runs once, succeeds, and then the controller has nothing to reconcile anymore.

### How our scenario breaks the one-shot guarantee

Two things go wrong:

**Problem 1 — `Job.Status.Succeeded` is a false-positive success.** The klipper-helm wrapper ends with `helm uninstall … || true`, so even when `helm uninstall` errors out (our `already exists` case), the script exits 0 and `Succeeded` becomes 1. OnRemove can't tell a real success from a false one — it trusts the Job status and proceeds past L431 as if the uninstall worked, when in fact the release is still in `uninstalling` state and the chart resources are still in the cluster.

**Problem 2 — L446 issues Delete on the Job, but the HelmChart isn't being finalized away cleanly.** In the happy path, L446 returns nil → L456 → framework clears the HelmChart finalizer → HelmChart is gone **before any further reconcile can run**, so the Terminating helm-delete Job becomes irrelevant (no HelmChart to fire OnRemove against). In our scenario the bundle's evidence (recurring `"Replace Wait"` lines, second helm-delete Job at 23:12:15) tells us OnRemove **is** firing again after the success path runs. Two candidate explanations both fit:

- **L446 returns an error** (e.g., `"Replace Wait"` because wrangler waits for the Job to actually be gone before reporting the empty apply complete). OnRemove returns `(nil, err)` at [L453](helm-controller/pkg/controllers/chart/chart.go#L453), the framework doesn't clear the HelmChart finalizer, and the controller is requeued. On the next pass the helm-delete Job may already be gone (finalizer cleared by `OnJobRemove`), so L390-L395 creates a fresh Job — back to call #1.
- **L446 returns nil**, but events from the Job's deletion / re-creation arrive in the workqueue **before** the framework finishes clearing the HelmChart finalizer, so a fresh OnRemove starts on the still-finalizer-blocked HelmChart. Same outcome: L390-L395 creates a fresh Job.

Either way, the **load-bearing missing piece** is "HelmChart actually gets deleted after a success." If that happened, the controller would have nothing left to reconcile and the loop would end. It doesn't happen because the wrapper-script-induced false success keeps OnRemove looping through L446 → wait → L390 → … forever instead of cleanly terminating on a real success.

### Why the fix works

`before-hook-creation,hook-succeeded` restores the one-shot property by making `helm uninstall` **actually** succeed instead of false-succeed:

1. Next helm-delete pod runs `helm uninstall`.
2. Helm sees the existing hook Job from a previous attempt, **deletes it first** (because of `before-hook-creation`), then creates a fresh one.
3. New hook Job runs, succeeds. Helm cleans it up (`hook-succeeded`).
4. Helm continues, deletes main resources, marks release `uninstalled`, pod exits 0 — **real success this time, not `|| true`-masked**.
5. OnRemove sees `Succeeded == 1`, runs L446 empty apply, returns nil at L456.
6. Framework clears the HelmChart finalizer. HelmChart is deleted. No more OnRemove calls. **Loop ends.**

## The load-bearing question: why was `b5hxg` killed?

Everything downstream of this investigation hinges on **one specific moment**: at 23:09:15, kubelet stopped `b5hxg`'s container while Helm was blocked waiting for the pre-delete hook to finish. If `b5hxg` had been allowed to run to completion, it would have cleaned up the hook Job (`hook-succeeded` policy), finished the uninstall, and the HelmChart would already be gone. **No loop, no leftover hook Job, no stuck deletion.** Every step from 4 onward in "The complete picture" is just a consequence of this one event.

### What the bundle proves

From [events.yaml#L16908-L16954](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/v1/events.yaml#L16908-L16954): kubelet emitted `Killing "Stopping container helm"` for `b5hxg` at 23:09:15. The `Killing` reason means kubelet was *responding* to a deletion — either of the pod itself or of its parent Job (cascade) — not initiating one. The Job's `restartPolicy: OnFailure` rules out "container exited non-zero → kubelet gave up": kubelet would have restarted the container in place, not stopped it.

So **something issued a `DELETE` on either the pod or its parent Job between 23:09:12 (`Started`) and 23:09:15 (`Killing`)**. The bundle doesn't capture *what* issued that delete.

### Candidate causes, ranked by fit

1. **Parent Job replaced by wrangler at ~23:09:14.** This is the tightest fit with the `"DesiredSet - Replace Wait"` log line at exactly [23:09:14.631](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3760). If wrangler's apply decided to replace the helm-delete Job (whether because of an unexpected `templateChanged → ErrReplace`, a stale-cache race, or some path I haven't identified), the Job gets `deletionTimestamp` → cascade-deletes `b5hxg` → kubelet emits `Killing`. The replacement pod `45qjv` would then be owned by the **new** Job, not the old one.
2. **The install→delete swap was still in flight.** If the install Job's `wrangler.cattle.io/promote-node-controller` finalizer took several seconds to clear (delaying the install Job's actual removal until ~23:09:14), wrangler's `Replace Wait` at 23:09:14.631 is the apply finally completing its delete-then-create of the helm-delete Job. But that doesn't explain why a `b5hxg` pod (already created at 23:09:11) would be killed.
3. **Direct pod deletion by another controller.** No known controller in Harvester or RKE2 targets these pods individually.
4. **Node-level cause** (eviction / preemption / OOM). Ruled out — those would surface as `Evicted`, `Preempted`, or `OOMKilled` events, not plain `Killing`.

### What we'd need to be definitive

The bundle is missing exactly the artifacts that would resolve this:

- The `ownerReferences[0].uid` of `b5hxg` (would prove whether its parent Job is the same one that created `45qjv`).
- A Kubernetes audit log entry for the `DELETE` at ~23:09:14.
- helm-controller / wrangler debug log lines around 23:09:14 showing whether `reconcileJob` returned `ErrReplace`.

None of these is in the support bundle. **Pinning down the root cause requires reproducing the issue with audit-log capture enabled.**

### Why the fix still works without knowing the root cause

The proposed hook-policy change addresses the **proximate cause** (orphaned hook Job blocks subsequent uninstalls), which is downstream of whatever killed `b5hxg`. Even if a future helm-delete pod gets killed for the same unknown reason and leaves another orphan, the next attempt will clean it up (`before-hook-creation`) and proceed. So the fix is robust to the root cause remaining a mystery.

## Fix

Change the hook annotations in [`charts/charts/harvester-csi-driver-lvm/templates/uninstall-hook.yaml`](charts/charts/harvester-csi-driver-lvm/templates/uninstall-hook.yaml#L5-L7) to:

```yaml
metadata:
  annotations:
    "helm.sh/hook": pre-delete
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
```

Why both:

- `hook-succeeded` — happy-path cleanup (existing behavior).
- `before-hook-creation` — safety net. If Helm ever crashes between hook success and cleanup (as happened here), the next uninstall will delete the leftover Job before creating a new one instead of dead-locking on `already exists`. The `promote-node-controller` finalizer doesn't block this for long — its [`OnJobRemove`](pkg/controller/master/node/promote_controller.go#L173-L199) returns `nil` immediately for non-promote Jobs, so wrangler clears the finalizer in seconds.

Optionally also set `spec.ttlSecondsAfterFinished: 60` on the hook Job so Kubernetes' [TTL controller](https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/) garbage-collects a succeeded hook Job even if Helm never gets around to it.

> Scope note: this fix addresses the **proximate cause** (orphan hook Job blocking subsequent uninstalls), not the **root cause** of why the very first helm-delete pod `b5hxg` was killed mid-hook. See "The load-bearing question" above for what we know and don't know about that root cause. The hook-policy change is robust either way — if a future pod gets killed for the same unknown reason, the next attempt will clean up the orphan and proceed instead of dead-locking.

## Recovering the current stuck cluster

To unstick the cluster without a new chart release:

```bash
kubectl -n harvester-system delete job delete-harvester-csi-driver-lvm-webhook
```

After the leftover hook Job is gone, the next `helm-delete-…` pod will get past the pre-delete hook, `helm uninstall` will complete, helm-controller's `OnRemove` will return success, and the HelmChart (and its Addon owner) will finalize.

## References

Support bundle:

- HelmChart manifest: [helmcharts.yaml#L3](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/helm.cattle.io/v1/helmcharts.yaml#L3)
- Leftover pre-delete hook Job: [jobs.yaml#L157](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L157-L300)
- Parent helm-delete Job: [jobs.yaml#L914](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L914)
- Original helm-install Job: [jobs.yaml#L1547](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/batch/v1/jobs.yaml#L1547)
- Failing helm uninstall log: [helm.log#L27-L44](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/logs/harvester-system/helm-delete-harvester-csi-driver-lvm-sbf48/helm.log#L27-L44)
- rke2-server requeue log: [rke2-server.log#L3743-L3783](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/nodes/hp-117-tink-system/logs/rke2-server.log#L3743-L3783)
- Job lifecycle events (`SuccessfulCreate` / `Completed`): [events.yaml](supportbundle_0f1ba762-de89-4d47-a373-8b8e1a0fbd6f_2026-06-12T23-12-19Z/yamls/namespaced/harvester-system/v1/events.yaml#L17419)

Chart:

- Pre-delete hook template: [uninstall-hook.yaml](charts/charts/harvester-csi-driver-lvm/templates/uninstall-hook.yaml#L1-L24)

helm-controller:

- `jobSettleTime`: [chart.go#L78](helm-controller/pkg/controllers/chart/chart.go#L78)
- Apply registration with `reconcileJob`: [chart.go#L164](helm-controller/pkg/controllers/chart/chart.go#L164)
- `on-helm-chart-remove` registration: [chart.go#L186](helm-controller/pkg/controllers/chart/chart.go#L186)
- `reconcileJob`: [chart.go#L209-L224](helm-controller/pkg/controllers/chart/chart.go#L209-L224)
- `OnRemove` apply with `helm-chart-registration` set: [chart.go#L394](helm-controller/pkg/controllers/chart/chart.go#L394) and [chart.go#L416](helm-controller/pkg/controllers/chart/chart.go#L416)
- `waiting for delete of helm chart` error: [chart.go#L439](helm-controller/pkg/controllers/chart/chart.go#L439)
- `templateChanged`: [chart.go#L1345-L1349](helm-controller/pkg/controllers/chart/chart.go#L1345-L1349)

Harvester promote-node-controller:

- Name + `OnRemove` registration: [promote_controller.go#L32](pkg/controller/master/node/promote_controller.go#L32) / [promote_controller.go#L95](pkg/controller/master/node/promote_controller.go#L95)
- `OnJobRemove` body: [promote_controller.go#L173-L199](pkg/controller/master/node/promote_controller.go#L173-L199)

External:

- [Helm hook deletion policies](https://helm.sh/docs/topics/charts_hooks/#hook-deletion-policies)
- [Kubernetes Job TTL after finished](https://kubernetes.io/docs/concepts/workloads/controllers/ttlafterfinished/)
