# ADR-002: Replace Helm Hook Migrations with Operator-Managed Migrations

- **Status:** Proposed
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related ADR:** [ADR-001](001-adopt-openfga-operator.md)
- **Related Issues:** [#211](https://github.com/openfga/helm-charts/issues/211), [#107](https://github.com/openfga/helm-charts/issues/107), [#120](https://github.com/openfga/helm-charts/issues/120), [#100](https://github.com/openfga/helm-charts/issues/100), [#95](https://github.com/openfga/helm-charts/issues/95), [#126](https://github.com/openfga/helm-charts/issues/126), [#132](https://github.com/openfga/helm-charts/issues/132), [#144](https://github.com/openfga/helm-charts/issues/144)

## Context

### How Migrations Work Today

With the operator disabled (the default), the current Helm chart's legacy Job mode uses a **Helm hook Job** to run database migrations (`openfga migrate`) and, when `datastore.waitForMigrations` is enabled, a **`k8s-wait-for` init container** on the Deployment to block server startup until the migration completes.

Seven files are involved:

| File | Role |
|------|------|
| `charts/openfga/templates/job.yaml` | Migration Job with Helm hook annotations |
| `charts/openfga/templates/deployment.yaml` | OpenFGA Deployment + `wait-for-migration` init container |
| `charts/openfga/templates/serviceaccount.yaml` | Normal workload ServiceAccount, dedicated hook-managed migration ServiceAccount for eligible external-secret pre-hooks, and separately configurable operator migration ServiceAccount |
| `charts/openfga/templates/rbac.yaml` | Role + RoleBinding so init container can poll Job status |
| `charts/openfga/templates/_helpers.tpl` | Datastore environment variable helpers |
| `charts/openfga/values.yaml` | `datastore.*`, `migrate.*`, `initContainer.*` configuration |
| `charts/openfga/Chart.yaml` | `bitnami/common` dependency for migration sidecars |

**The migration Job** (`charts/openfga/templates/job.yaml`) is annotated as a Helm hook. For static credentials, chart-created Secrets, and in-release databases, the historical post-hook default is:

```yaml
annotations:
  "helm.sh/hook": post-install,post-upgrade,post-rollback,post-delete
  "helm.sh/hook-delete-policy": before-hook-creation
  "helm.sh/hook-weight": "-5"
```

When the datastore URI comes from `datastore.uriSecret`, or from `datastore.existingSecret` plus `datastore.secretKeys.uriKey`, and both bundled database subcharts are disabled, the chart instead selects `pre-install,pre-upgrade` and creates a dedicated hook-managed migration ServiceAccount ordered before the Job. This avoids the post-hook `--wait` deadlock for externally reachable databases without changing the runtime ServiceAccount lifecycle. Both variants remain Helm hooks outside the normal release lifecycle: the post-hook variant runs after regular resources, while the external-secret variant runs before them.

**The wait-for init container** blocks the Deployment pods from starting:

```yaml
initContainers:
  - name: wait-for-migration
    image: "groundnuty/k8s-wait-for:v2.0"
    args: ["job-wr", "<fullname>-migrate"]
```

It polls the Kubernetes API for the release-derived `<fullname>-migrate` Job until `.status.succeeded >= 1`. This requires RBAC permissions (Role/RoleBinding for `batch/jobs` `get`/`list`).

**The alternative mode** (`datastore.migrationType: initContainer`) runs migration directly inside each Deployment pod as an init container, avoiding hooks entirely but introducing redundant migration runs across replicas.

### The Six Tracked Issues

| Issue | Tool | Root Cause |
|-------|------|-----------|
| [**#211**](https://github.com/openfga/helm-charts/issues/211) | ArgoCD | ArgoCD and Helm hooks have incompatible lifecycle semantics. In the post-hook case, the init container can wait for a Job that the deployment workflow has not created as a regular managed resource. |
| [**#107**](https://github.com/openfga/helm-charts/issues/107) | ArgoCD | The hook Job is not a stable application resource in the GitOps desired state, which makes it difficult to observe, debug, or sync through ArgoCD. |
| [**#120**](https://github.com/openfga/helm-charts/issues/120) | Helm `--wait` | In the post-hook case, Helm waits for the Deployment to be ready before running post-install hooks. The Deployment is never ready because the init container waits for the hook Job. The Job never runs because Helm is waiting. The external-secret pre-hook exception avoids this specific cycle when its prerequisites are met. |
| [**#100**](https://github.com/openfga/helm-charts/issues/100) | FluxCD | FluxCD waits for all resources by default. The `hook-delete-policy: before-hook-creation` removes the completed Job before FluxCD can confirm the Deployment is healthy. |
| [**#95**](https://github.com/openfga/helm-charts/issues/95) | AWS IRSA | Legacy post-hook migrations share the workload ServiceAccount. Eligible external-secret pre-hooks and the operator path use dedicated migration accounts, but the operator Job still inherits datastore environment and volume configuration from the runtime Deployment. A separate migration database URI remains future work. |
| [**#126**](https://github.com/openfga/helm-charts/issues/126) | All | The `k8s-wait-for` image is configured in two separate places in `charts/openfga/values.yaml`, leading to inconsistency. Related: [#132](https://github.com/openfga/helm-charts/issues/132) tracks reported vulnerabilities and [#144](https://github.com/openfga/helm-charts/issues/144) tracks the mutable tag. |

### Why Helm Hooks Are Fundamentally Wrong for This

Helm hooks are a **deploy-time orchestration mechanism**. They assume Helm is the active agent running the deployment. GitOps tools (ArgoCD, FluxCD) break this assumption — they render the chart to manifests and apply them declaratively. Whether the chart selects the post-hook default or the external-secret pre-hook exception, hook annotations can be ignored or translated differently by GitOps controllers and can cause ordering or cleanup conflicts.

This is not a bug in ArgoCD or FluxCD. It is a fundamental mismatch between Helm's imperative hook model and the declarative GitOps model.

## Decision

When `operator.enabled: true`, replace the Helm hook migration Job and chart-generated `k8s-wait-for` init container with **operator-managed migrations** as part of Stage 1 of the OpenFGA Operator (see [ADR-001](001-adopt-openfga-operator.md)). The legacy hook path remains the default while the operator is opt-in.

### How It Works

The operator runs a **migration controller** that reconciles the OpenFGA Deployment:

```
┌──────────────────────────────────────────────────────────┐
│                  Operator Reconciliation                 │
│                                                          │
│  1. Read Deployment → extract image tag (e.g. v1.14.0)   │
│  2. Read ConfigMap/<deployment-name>-migration-status    │
│     └── "Last migrated version: v1.13.0"                 │
│  3. Versions differ → migration needed                   │
│  4. Create Job/<deployment-name>-migrate                 │
│     ├── ServiceAccount: <fullname>-migration or custom   │
│     ├── Image: openfga/openfga:v1.14.0                   │
│     ├── Args: ["migrate"]                                │
│     └── ttlSecondsAfterFinished: 300                     │
│  5. Watch Job until succeeded                            │
│  6. Update ConfigMap → "version: v1.14.0"                │
│  7. Ensure Deployment is at desired replicas             │
│     (fresh install: 0 → N; upgrade: already running)     │
│  8. New pods pass readiness, serve requests              │
└──────────────────────────────────────────────────────────┘
```

**Key design decisions within this approach:**

#### Fresh-install scale gating and upgrade readiness

On **fresh install**, the Helm chart renders the Deployment with `replicas: 0` (no existing Deployment found via `lookup`). The operator runs the migration Job and scales the Deployment to the desired replica count afterward. The operator does not directly set a Deployment readiness condition.

On **upgrade**, the chart uses Helm's `lookup` function to read the current replica count from the live Deployment and preserves it. Kubernetes starts a rolling update with the new image. OpenFGA has a **built-in schema version gate**: on startup, each instance calls `IsReady()` and checks the database schema revision against `MinimumSupportedDatastoreSchemaRevision` (via goose). If the schema is behind, the gRPC health endpoint returns `NOT_SERVING`. With the chart's default gRPC readiness probe, or an equivalent custom probe that honors this response, Kubernetes keeps the new pod out of service while old pods continue serving on the compatible schema. Once the operator's migration Job completes, new pods pass readiness and the rolling update proceeds.

The operator does not scale an existing Deployment to zero during upgrades; it relies on the application readiness check for rollout safety. Disabling `readinessProbe` or replacing it with a custom probe that does not honor OpenFGA's `NOT_SERVING` response removes this protection and invalidates the zero-downtime guarantee. The previous approach (always starting at `replicas: 0`) introduced a full outage on every `helm upgrade`, even for config-only changes.

**`lookup` caveat:** `helm template` and `--dry-run=client` cannot query the cluster, so `lookup` returns empty and the template falls back to `replicas: 0`. This is correct for CI rendering (no live cluster) and does not affect real installs/upgrades. `--dry-run=server` works correctly.

#### Version tracking via ConfigMap

A ConfigMap named `<deployment-name>-migration-status` stores the latest successful migration status as `version`, `migratedAt`, and `jobName`. Each success overwrites those fields, so the ConfigMap is not a migration history or audit trail. The operator compares the stored version to the Deployment's image tag or digest to determine if migration is needed. This is:
- Simple to inspect (`kubectl get configmap <deployment-name>-migration-status -o yaml`)
- Survives operator restarts
- Rebuilt from a matching completed Job if the ConfigMap is deleted while that Job still exists

Deleting the ConfigMap alone therefore does not reliably force a migration. To force a rerun while the completed Job still exists, delete `<deployment-name>-migrate` first and then delete `<deployment-name>-migration-status`; if the Job has already been garbage-collected, deleting the ConfigMap is sufficient.

#### Separate ServiceAccount for migrations

By default, the operator path creates a dedicated `<fullname>-migration` ServiceAccount for migration Jobs. Users can annotate it with cloud IAM roles that grant DDL permissions while the runtime ServiceAccount retains only CRUD permissions. When `migration.serviceAccount.create: false`, the chart requires `migration.serviceAccount.name` and uses that pre-existing account instead. This separates workload identity, but the operator Job still inherits datastore `Env` and `EnvFrom`, volumes, volume mounts, and resources from the runtime Deployment. A separate migration database URI and full migration-specific environment and volume configuration remain future work under [#95](https://github.com/openfga/helm-charts/issues/95).

#### Migration Job is a regular resource

The Job created by the operator has no Helm hook annotations. It is a standard Kubernetes Job that is inspectable through the Kubernetes API with tools such as `kubectl`. Because it is created dynamically after Helm rendering, it is not part of the chart or HelmRelease desired manifests and is not an ArgoCD- or Flux-managed application resource. Its owner reference points to the OpenFGA Deployment for garbage collection.

#### Failure handling

| Failure | Behavior |
|---------|----------|
| Job fails | Operator sets a `MigrationFailed` condition, records a retry time, and deletes the failed Job before the 60-second retry. A fresh install remains at zero replicas; an upgrade keeps its existing replica count. Inspect the Deployment condition and operator logs; use centralized pod logging if failed migration logs must survive Job deletion. |
| Job hangs | `activeDeadlineSeconds` (default 300s) terminates it; the operator handles it as a failure and deletes it before retry. |
| Operator crashes | On restart, re-reads ConfigMap and Job status. Resumes from where it left off. |
| Database unreachable | Job fails to connect. After exhausting `backoffLimit`, operator deletes the failed Job, sets a `retry-after` annotation, and recreates a fresh Job after a fixed 60-second cooldown. Cycle repeats until the database becomes available. |

### Sequence Comparison

**Before (legacy post-hook case for static/in-release credentials):**

```
helm install
  ├── Create ServiceAccount, RBAC, Secret, Service
  ├── Create Deployment (with wait-for-migration init container)
  │     └── Pod starts → init container polls for Job → waits...
  ├── [Helm finishes regular resources]
  ├── Run post-install hooks:
  │     └── Create Job/<fullname>-migrate → runs openfga migrate
  │           └── Job succeeds
  ├── Init container sees Job succeeded → exits
  └── Main container starts
```

Problems: hook handling differs across GitOps tools, and `--wait` deadlocks between steps 2 and 4. Eligible externally reachable datastores whose URI comes from an external Secret instead use pre-install/pre-upgrade hooks, which avoid this specific post-hook deadlock but remain outside the regular desired-resource lifecycle.

**After (operator-managed, fresh install):**

```
helm install
  ├── Create runtime ServiceAccount and, when requested, <fullname>-migration
  ├── Create Secret, Service
  ├── Create Deployment (replicas: 0 via lookup fallback, no chart-generated migration init containers)
  ├── Create Operator Deployment
  └── [Helm is done — all resources are regular, no hooks]

Operator starts:
  ├── Detects Deployment image version
  ├── No migration status ConfigMap → migration needed
  ├── Creates Job/<deployment-name>-migrate (regular Job, no hooks)
  │     └── Uses <fullname>-migration or the configured existing account
  │     └── Runs openfga migrate → succeeds
  ├── Creates ConfigMap/<deployment-name>-migration-status
  └── Scales Deployment 0 → 3 replicas → pods start
```

**After (operator-managed, upgrade with new image):**

```
helm upgrade
  ├── lookup finds existing Deployment at 3 replicas → preserves replicas: 3
  ├── Patches Deployment with new image tag
  ├── Kubernetes starts rolling update
  │     ├── New pods (v1.14) start → schema is behind →
  │     │   schema-aware readiness fails (gRPC NOT_SERVING) → no traffic routed
  │     └── Old pods (v1.13) continue serving traffic
  └── [Helm is done]

Operator reconciles:
  ├── Detects image version differs from ConfigMap
  ├── Creates Job/<deployment-name>-migrate → runs migration
  ├── Updates ConfigMap → "version: v1.14.0"
  └── New pods pass readiness → rolling update completes
      (operator does not scale to zero; rollout safety requires a
       schema-aware readiness probe)
```

No Helm migration hooks, no chart-generated migration init containers, and no `k8s-wait-for` in operator mode. User-supplied `extraInitContainers` remain. The dynamically created Job is a regular Kubernetes object but is not a GitOps-managed desired resource. Upgrades avoid scaling to zero, while zero-downtime routing depends on a schema-aware readiness probe.

### What Changes in the Helm Chart

Nothing is deleted outright — every change is gated on `operator.enabled` so the legacy flow remains the default for backward compatibility.

**Gated on `operator.enabled: false` (legacy Helm-hook flow, rendered when the operator is disabled):**

| File/Section | Behavior when operator is enabled |
|--------------|-----------------------------------|
| `charts/openfga/templates/job.yaml` | Skipped — operator creates migration Jobs dynamically |
| `charts/openfga/templates/rbac.yaml` | Skipped — no init container needs to poll Job status |
| `charts/openfga/values.yaml`: `initContainer.*` | Unused — `k8s-wait-for` not deployed |
| `charts/openfga/values.yaml`: `datastore.applyMigrations`, `datastore.migrationType`, `datastore.waitForMigrations` | Ignored in operator mode — use `migration.enabled: false` when migrations are managed externally |
| `charts/openfga/values.yaml`: `migrate.annotations` | Unused — no Helm hooks |
| `charts/openfga/values.yaml`: `migrate.extraVolumes`, `migrate.extraVolumeMounts` | Ignored — the operator inherits the main Deployment's volumes and container mounts; use top-level `extraVolumes` and `extraVolumeMounts` |
| Chart-generated Deployment migration init containers | Skipped — fresh installs use replica gating and upgrades rely on application readiness; user-supplied `extraInitContainers` are preserved |

**Added (active only when `operator.enabled: true`):**

| File/Section | Purpose |
|--------------|---------|
| `charts/openfga/values.yaml`: `operator.enabled` | Toggle the operator subchart |
| `charts/openfga/values.yaml`: `migration.enabled` | Toggle operator-managed migrations; disable it for externally managed migrations |
| `charts/openfga/values.yaml`: `migration.serviceAccount.*` | Create the dedicated migration ServiceAccount or name a pre-existing one |
| `charts/openfga/values.yaml`: `openfga-operator.migrationJob.*` | Configure `backoffLimit`, `activeDeadlineSeconds`, and `ttlSecondsAfterFinished` in the dependent operator chart |
| `charts/openfga/templates/serviceaccount.yaml`: operator migration SA | Separately configurable operator migration ServiceAccount, rendered only when `migration.serviceAccount.create: true`; eligible external-secret pre-hooks use a different hook-managed migration account |
| `charts/openfga-operator/` | Operator subchart (conditional dependency) |

Users on `operator.enabled: false` (the default) retain the non-operator migration flow, so gradual adoption is possible with no forced migration. The current legacy flow is not byte-for-byte identical to older chart releases: it now selects pre-install/pre-upgrade hooks and creates a dedicated hook-managed migration ServiceAccount for eligible external-secret datastores, while other Job-mode configurations retain the post-hook behavior and workload ServiceAccount.

## Consequences

### Positive

- **Five migration issues resolved on the operator path:** [#211](https://github.com/openfga/helm-charts/issues/211), [#107](https://github.com/openfga/helm-charts/issues/107), [#120](https://github.com/openfga/helm-charts/issues/120), [#100](https://github.com/openfga/helm-charts/issues/100), and [#126](https://github.com/openfga/helm-charts/issues/126). [#95](https://github.com/openfga/helm-charts/issues/95) is partially addressed through the separate operator migration ServiceAccount.
- **`k8s-wait-for` eliminated from the operator-enabled path:** removes an inactively maintained image with [reported vulnerabilities](https://github.com/openfga/helm-charts/issues/132) and a [mutable tag](https://github.com/openfga/helm-charts/issues/144) from that deployment mode
- **Workload identity separation improved:** a chart-created or pre-existing migration ServiceAccount can hold DDL permissions separately from the runtime account's CRUD permissions. The migration datastore environment and volume configuration are still inherited, so separate migration credentials are not yet fully supported.
- **Runtime surface area reduced** — when `operator.enabled: true`, the legacy migration Job, init-container `k8s-wait-for` logic, and job-watching RBAC are skipped from the rendered manifest
- **Migration is observable through Kubernetes:** the dynamically created Job is inspectable through the Kubernetes API, the ConfigMap stores only the latest `version`, `migratedAt`, and `jobName`, and operator conditions surface errors. The ConfigMap is not an audit trail, and the Job is not tracked as a GitOps desired resource.
- **Idempotent and crash-safe** — operator can restart at any point and resume correctly

### Negative

- **Operator is a new runtime dependency** — if the operator pod is unavailable, migrations don't run (but existing running pods are unaffected)
- **`lookup` limitation** — `helm template` and `--dry-run=client` cannot query the cluster; the template falls back to `replicas: 0` in these contexts. This does not affect real installs/upgrades.
- **Two upgrade paths to document** — `operator.enabled: true` (new) vs `operator.enabled: false` (legacy)

### Risks

- **Upgrade safety depends on schema-aware readiness** — the zero-downtime upgrade model depends on `MinimumSupportedDatastoreSchemaRevision` causing `NOT_SERVING` when the schema is behind and on a readiness probe that honors that response. Disabling the default probe, replacing it with a probe that does not check OpenFGA readiness, or weakening the application check could route traffic to a pod before migration completes.
- **ConfigMap and completed Job jointly represent state** — deleting the ConfigMap while the matching completed Job exists causes the operator to rebuild the ConfigMap rather than rerun migration. Operators must delete the completed Job first and then the ConfigMap to force a rerun.
- **Mutable image tags can skip migrations** — the operator compares the extracted image tag or digest with the recorded version. Reusing a tag such as `latest` for a different image does not produce a version change, so use immutable tags or digests.
