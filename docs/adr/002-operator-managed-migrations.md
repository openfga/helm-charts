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
┌────────────────────────────────────────────────────────┐
│                  Operator Reconciliation                │
│                                                        │
│  1. Read Deployment → extract image tag (e.g. v1.14.0) │
│  2. Read ConfigMap/openfga-migration-status             │
│     └── "Last migrated version: v1.13.0"               │
│  3. Versions differ → migration needed                  │
│  4. Create Job/openfga-migrate                          │
│     ├── ServiceAccount: openfga-migrator (DDL perms)   │
│     ├── Image: openfga/openfga:v1.14.0                 │
│     ├── Args: ["migrate"]                              │
│     └── ttlSecondsAfterFinished: 300                   │
│  5. Watch Job until succeeded                           │
│  6. Update ConfigMap → "version: v1.14.0"              │
│  7. Scale Deployment replicas: 0 → 3                   │
│  8. OpenFGA pods start, serve requests                  │
└────────────────────────────────────────────────────────┘
```

**Key design decisions within this approach:**

#### Deployment starts at replicas: 0

The Helm chart renders the Deployment with `replicas: 0` when `operator.enabled: true`. The operator scales it up only after migration succeeds. This is simpler than readiness gates or admission webhooks, and ensures no pods run against an unmigrated schema.

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

**After (operator-managed):**

```
helm install
  ├── Create ServiceAccount (runtime), ServiceAccount (migrator)
  ├── Create Secret, Service
  ├── Create Deployment (replicas: 0, no init containers)
  ├── Create Operator Deployment
  └── [Helm is done — all resources are regular, no hooks]

Operator starts:
  ├── Detects Deployment image version
  ├── No migration status ConfigMap → migration needed
  ├── Creates Job/openfga-migrate (regular Job, no hooks)
  │     └── Uses openfga-migrator ServiceAccount
  │     └── Runs openfga migrate → succeeds
  ├── Creates ConfigMap with migrated version
  └── Scales Deployment to 3 replicas → pods start
```

No hooks. No init containers. No `k8s-wait-for`. All resources are regular Kubernetes objects.

### What Changes in the Helm Chart

**Removed:**

| File/Section | Reason |
|--------------|--------|
| `templates/job.yaml` | Operator creates migration Jobs |
| `templates/rbac.yaml` | No init container polling Job status |
| `values.yaml`: `initContainer.repository`, `initContainer.tag` | `k8s-wait-for` eliminated |
| `values.yaml`: `datastore.migrationType` | Operator always uses Job internally |
| `values.yaml`: `datastore.waitForMigrations` | Operator handles ordering |
| `values.yaml`: `migrate.annotations` (hook annotations) | No Helm hooks |
| Deployment init containers for migration | Operator manages readiness via replica scaling |

**Added:**

| File/Section | Purpose |
|--------------|---------|
| `values.yaml`: `operator.enabled` | Toggle operator subchart |
| `values.yaml`: `migration.serviceAccount.*` | Separate ServiceAccount for migration Jobs |
| `values.yaml`: `migration.timeout`, `backoffLimit`, `ttlSecondsAfterFinished` | Migration Job configuration |
| `templates/serviceaccount.yaml`: second SA | Migration ServiceAccount |
| `charts/openfga-operator/` | Operator subchart |

**Preserved (backward compatible):**

When `operator.enabled: false`, the chart falls back to the current behavior — Helm hooks, `k8s-wait-for` init container, shared ServiceAccount. This allows gradual adoption.

## Consequences

### Positive

- **All 6 migration issues resolved** — no Helm hooks means no ArgoCD/FluxCD/`--wait` incompatibility
- **`k8s-wait-for` eliminated** — removes an unmaintained image with CVEs from the supply chain (#132, #144)
- **Least-privilege enforced** — separate ServiceAccounts for migration (DDL) and runtime (CRUD) (#95)
- **Helm chart simplified** — 2 templates removed, init container logic removed, RBAC for job-watching removed
- **Migration is observable** — Job is a regular resource visible in all tools; ConfigMap records migration history; operator conditions surface errors
- **Idempotent and crash-safe** — operator can restart at any point and resume correctly

### Negative

- **Operator is a new runtime dependency** — if the operator pod is unavailable, migrations don't run (but existing running pods are unaffected)
- **Replica scaling model** — starting at `replicas: 0` means a brief period where the Deployment exists but has no pods; monitoring tools may flag this
- **Two upgrade paths to document** — `operator.enabled: true` (new) vs `operator.enabled: false` (legacy)

### Risks

- **Zero-downtime upgrades** — the initial implementation scales to 0 during migration, causing brief downtime. A future enhancement can support rolling upgrades where the new schema is backward-compatible, but this is explicitly out of scope for Stage 1.
- **ConfigMap as state store** — if the ConfigMap is accidentally deleted, the operator re-runs migration (which is safe — `openfga migrate` is idempotent). This is a feature, not a bug, but should be documented.
