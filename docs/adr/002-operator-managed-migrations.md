# ADR-002: Replace Helm Hook Migrations with Operator-Managed Migrations

- **Status:** Proposed
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related ADR:** [ADR-001](001-adopt-openfga-operator.md)
- **Related Issues:** #211, #107, #120, #100, #95, #126, #132, #144

## Context

### How Migrations Work Today

The current Helm chart uses a **Helm hook Job** to run database migrations (`openfga migrate`) and a **`k8s-wait-for` init container** on the Deployment to block server startup until the migration completes.

Seven files are involved:

| File | Role |
|------|------|
| `templates/job.yaml` | Migration Job with Helm hook annotations |
| `templates/deployment.yaml` | OpenFGA Deployment + `wait-for-migration` init container |
| `templates/serviceaccount.yaml` | Shared ServiceAccount (migration + runtime) |
| `templates/rbac.yaml` | Role + RoleBinding so init container can poll Job status |
| `templates/_helpers.tpl` | Datastore environment variable helpers |
| `values.yaml` | `datastore.*`, `migrate.*`, `initContainer.*` configuration |
| `Chart.yaml` | `bitnami/common` dependency for migration sidecars |

**The migration Job** (`templates/job.yaml`) is annotated as a Helm hook:

```yaml
annotations:
  "helm.sh/hook": post-install,post-upgrade,post-rollback,post-delete
  "helm.sh/hook-delete-policy": before-hook-creation
  "helm.sh/hook-weight": "1"
```

This means Helm manages it outside the normal release lifecycle — it only runs after Helm finishes creating/upgrading all other resources.

**The wait-for init container** blocks the Deployment pods from starting:

```yaml
initContainers:
  - name: wait-for-migration
    image: "groundnuty/k8s-wait-for:v2.0"
    args: ["job-wr", "openfga-migrate"]
```

It polls the Kubernetes API (`GET /apis/batch/v1/.../jobs/openfga-migrate`) until `.status.succeeded >= 1`. This requires RBAC permissions (Role/RoleBinding for `batch/jobs` `get`/`list`).

**The alternative mode** (`datastore.migrationType: initContainer`) runs migration directly inside each Deployment pod as an init container, avoiding hooks entirely but introducing redundant migration runs across replicas.

### The Six Issues

| Issue | Tool | Root Cause |
|-------|------|-----------|
| **#211** | ArgoCD | ArgoCD ignores Helm hook annotations. The migration Job is never created as a managed resource. The init container waits forever for a Job that doesn't exist. |
| **#107** | ArgoCD | Same root cause. The Job is invisible in ArgoCD's UI — users can't see, debug, or manually sync it. |
| **#120** | Helm `--wait` | Circular deadlock. Helm waits for the Deployment to be ready before running post-install hooks. The Deployment is never ready because the init container waits for the hook Job. The Job never runs because Helm is waiting. |
| **#100** | FluxCD | FluxCD waits for all resources by default. The `hook-delete-policy: before-hook-creation` removes the completed Job before FluxCD can confirm the Deployment is healthy. |
| **#95** | AWS IRSA | Migration and runtime share a ServiceAccount. With IAM-based DB auth, the runtime gets DDL permissions it doesn't need (CREATE TABLE, ALTER TABLE). |
| **#126** | All | The `k8s-wait-for` image is configured in two separate places in `values.yaml`, leading to inconsistency. Related: #132 (image unmaintained, has CVEs) and #144 (pinned by mutable tag). |

### Why Helm Hooks Are Fundamentally Wrong for This

Helm hooks are a **deploy-time orchestration mechanism**. They assume Helm is the active agent running the deployment. GitOps tools (ArgoCD, FluxCD) break this assumption — they render the chart to manifests and apply them declaratively. The hook annotations are either ignored (ArgoCD) or cause ordering/cleanup conflicts (FluxCD).

This is not a bug in ArgoCD or FluxCD. It is a fundamental mismatch between Helm's imperative hook model and the declarative GitOps model.

## Decision

Replace the Helm hook migration Job and `k8s-wait-for` init container with **operator-managed migrations** as part of Stage 1 of the OpenFGA Operator (see [ADR-001](001-adopt-openfga-operator.md)).

### How It Works

The operator runs a **migration controller** that reconciles the OpenFGA Deployment:

```
┌──────────────────────────────────────────────────────────┐
│                  Operator Reconciliation                  │
│                                                          │
│  1. Read Deployment → extract image tag (e.g. v1.14.0)   │
│  2. Read ConfigMap/openfga-migration-status               │
│     └── "Last migrated version: v1.13.0"                 │
│  3. Versions differ → migration needed                    │
│  4. Create Job/openfga-migrate                            │
│     ├── ServiceAccount: openfga-migrator (DDL perms)     │
│     ├── Image: openfga/openfga:v1.14.0                   │
│     ├── Args: ["migrate"]                                │
│     └── ttlSecondsAfterFinished: 300                     │
│  5. Watch Job until succeeded                             │
│  6. Update ConfigMap → "version: v1.14.0"                │
│  7. Ensure Deployment at desired replicas                 │
│     (fresh install: 0 → N; upgrade: already running)     │
│  8. New pods pass readiness, serve requests               │
└──────────────────────────────────────────────────────────┘
```

**Key design decisions within this approach:**

#### Zero-downtime upgrades via lookup and readiness gating

On **fresh install**, the Helm chart renders the Deployment with `replicas: 0` (no existing Deployment found via `lookup`). The operator runs the migration Job and scales the Deployment to the desired replica count afterward.

On **upgrade**, the chart uses Helm's `lookup` function to read the current replica count from the live Deployment and preserves it. Kubernetes starts a rolling update with the new image. OpenFGA has a **built-in schema version gate**: on startup, each instance calls `IsReady()` which checks the database schema revision against `MinimumSupportedDatastoreSchemaRevision` (via goose). If the schema is behind, the gRPC health endpoint returns `NOT_SERVING`, the readiness probe fails, and Kubernetes does not route traffic to the pod. Old pods continue serving on the migrated schema (OpenFGA migrations are additive/backward-compatible — this is how the existing Helm hook flow has operated for years with rolling updates). Once the operator's migration Job completes, new pods pass readiness and the rolling update proceeds.

This matches the existing zero-downtime behavior of the non-operator chart. The previous approach (always starting at `replicas: 0`) introduced a full outage on every `helm upgrade` — even for config-only changes — which was a regression from the existing rolling update model.

**`lookup` caveat:** `helm template` and `--dry-run=client` cannot query the cluster, so `lookup` returns empty and the template falls back to `replicas: 0`. This is correct for CI rendering (no live cluster) and does not affect real installs/upgrades. `--dry-run=server` works correctly.

#### Version tracking via ConfigMap

A ConfigMap (`openfga-migration-status`) records the last successfully migrated version. The operator compares this to the Deployment's image tag to determine if migration is needed. This is:
- Simple to inspect (`kubectl get configmap openfga-migration-status -o yaml`)
- Survives operator restarts
- Can be manually deleted to force re-migration

#### Separate ServiceAccount for migrations

The operator creates a dedicated `openfga-migrator` ServiceAccount for migration Jobs. Users can annotate it with cloud IAM roles that grant DDL permissions, while the runtime ServiceAccount retains only CRUD permissions.

#### Migration Job is a regular resource

The Job created by the operator has no Helm hook annotations. It is a standard Kubernetes Job, visible to ArgoCD, FluxCD, and all Kubernetes tooling. It has an owner reference to the operator's managed resource for proper garbage collection.

#### Failure handling

| Failure | Behavior |
|---------|----------|
| Job fails | Operator sets `MigrationFailed` condition on Deployment. Does NOT scale up. User inspects Job logs. |
| Job hangs | `activeDeadlineSeconds` (default 300s) kills it. Operator sees failure. |
| Operator crashes | On restart, re-reads ConfigMap and Job status. Resumes from where it left off. |
| Database unreachable | Job fails to connect. After exhausting `backoffLimit`, operator deletes the failed Job, sets a `retry-after` annotation, and recreates a fresh Job after a fixed 60-second cooldown. Cycle repeats until the database becomes available. |

### Sequence Comparison

**Before (Helm hooks):**

```
helm install
  ├── Create ServiceAccount, RBAC, Secret, Service
  ├── Create Deployment (with wait-for-migration init container)
  │     └── Pod starts → init container polls for Job → waits...
  ├── [Helm finishes regular resources]
  ├── Run post-install hooks:
  │     └── Create Job/openfga-migrate → runs openfga migrate
  │           └── Job succeeds
  ├── Init container sees Job succeeded → exits
  └── Main container starts
```

Problems: ArgoCD skips step 4. FluxCD deletes Job in step 4. `--wait` deadlocks between steps 2 and 4.

**After (operator-managed, fresh install):**

```
helm install
  ├── Create ServiceAccount (runtime), ServiceAccount (migrator)
  ├── Create Secret, Service
  ├── Create Deployment (replicas: 0 via lookup fallback, no init containers)
  ├── Create Operator Deployment
  └── [Helm is done — all resources are regular, no hooks]

Operator starts:
  ├── Detects Deployment image version
  ├── No migration status ConfigMap → migration needed
  ├── Creates Job/openfga-migrate (regular Job, no hooks)
  │     └── Uses openfga-migrator ServiceAccount
  │     └── Runs openfga migrate → succeeds
  ├── Creates ConfigMap with migrated version
  └── Scales Deployment 0 → 3 replicas → pods start
```

**After (operator-managed, upgrade with new image):**

```
helm upgrade
  ├── lookup finds existing Deployment at 3 replicas → preserves replicas: 3
  ├── Patches Deployment with new image tag
  ├── Kubernetes starts rolling update
  │     ├── New pods (v1.14) start → schema is behind →
  │     │   readiness fails (gRPC NOT_SERVING) → no traffic routed
  │     └── Old pods (v1.13) continue serving traffic
  └── [Helm is done]

Operator reconciles:
  ├── Detects image version differs from ConfigMap
  ├── Creates Job/openfga-migrate → runs migration
  ├── Updates ConfigMap → "version: v1.14.0"
  └── New pods pass readiness → rolling update completes
      (operator does NOT scale to zero — zero downtime)
```

No hooks. No init containers. No `k8s-wait-for`. No downtime on upgrade. All resources are regular Kubernetes objects.

### What Changes in the Helm Chart

Nothing is deleted outright — every change is gated on `operator.enabled` so the legacy flow remains the default for backward compatibility.

**Gated on `operator.enabled: false` (legacy Helm-hook flow, rendered when the operator is disabled):**

| File/Section | Behavior when operator is enabled |
|--------------|-----------------------------------|
| `templates/job.yaml` | Skipped — operator creates migration Jobs dynamically |
| `templates/rbac.yaml` | Skipped — no init container needs to poll Job status |
| `values.yaml`: `initContainer.*` | Unused — `k8s-wait-for` not deployed |
| `values.yaml`: `datastore.migrationType`, `datastore.waitForMigrations` | Unused — operator always uses a Job and handles ordering |
| `values.yaml`: `migrate.annotations` | Unused — no Helm hooks |
| Deployment migration init containers | Skipped — operator manages readiness via replica scaling |

**Added (active only when `operator.enabled: true`):**

| File/Section | Purpose |
|--------------|---------|
| `values.yaml`: `operator.enabled` | Toggle the operator subchart |
| `values.yaml`: `migration.serviceAccount.*` | Separate ServiceAccount for migration Jobs |
| `values.yaml`: `migration.backoffLimit`, `activeDeadlineSeconds`, `ttlSecondsAfterFinished` | Migration Job configuration |
| `templates/serviceaccount.yaml`: second SA | Migration ServiceAccount |
| `charts/openfga-operator/` | Operator subchart (conditional dependency) |

Users on `operator.enabled: false` (the default) see identical rendered output to the pre-operator chart, so gradual adoption is possible with no forced migration.

## Consequences

### Positive

- **All 6 migration issues resolved** — no Helm hooks means no ArgoCD/FluxCD/`--wait` incompatibility
- **`k8s-wait-for` eliminated** — removes an unmaintained image with CVEs from the supply chain (#132, #144)
- **Least-privilege enforced** — separate ServiceAccounts for migration (DDL) and runtime (CRUD) (#95)
- **Runtime surface area reduced** — when `operator.enabled: true`, the legacy migration Job, init-container `k8s-wait-for` logic, and job-watching RBAC are skipped from the rendered manifest
- **Migration is observable** — Job is a regular resource visible in all tools; ConfigMap records migration history; operator conditions surface errors
- **Idempotent and crash-safe** — operator can restart at any point and resume correctly

### Negative

- **Operator is a new runtime dependency** — if the operator pod is unavailable, migrations don't run (but existing running pods are unaffected)
- **`lookup` limitation** — `helm template` and `--dry-run=client` cannot query the cluster; the template falls back to `replicas: 0` in these contexts. This does not affect real installs/upgrades.
- **Two upgrade paths to document** — `operator.enabled: true` (new) vs `operator.enabled: false` (legacy)

### Risks

- **Readiness gate relies on OpenFGA's built-in schema check** — the zero-downtime upgrade model depends on `MinimumSupportedDatastoreSchemaRevision` in `pkg/storage/sqlcommon/sqlcommon.go` causing `NOT_SERVING` when the schema is behind. If a future OpenFGA release removes or weakens this check, new pods could serve traffic against an unmigrated schema. This coupling should be documented and monitored across OpenFGA releases.
- **ConfigMap as state store** — if the ConfigMap is accidentally deleted, the operator re-runs migration (which is safe — `openfga migrate` is idempotent). This is a feature, not a bug, but should be documented.
