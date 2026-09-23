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

```text
┌──────────────────────────────────────────────────────────┐
│                  Operator Reconciliation                 │
│                                                          │
│  1. Read Deployment → extract image tag (e.g. v1.14.0)   │
│  2. Read ConfigMap/openfga-migration-status              │
│     └── "Last migrated version: v1.13.0"                 │
│  3. Versions differ → migration needed                   │
│  4. Create Job/openfga-migrate                           │
│     ├── ServiceAccount: openfga-migrator (DDL perms)     │
│     ├── Image: openfga/openfga:v1.14.0                   │
│     ├── Args: ["migrate"]                                │
│     └── ttlSecondsAfterFinished: 300                     │
│  5. Watch Job until succeeded                            │
│  6. Update ConfigMap → "version: v1.14.0"                │
└──────────────────────────────────────────────────────────┘
```

**Key design decisions within this approach:**

#### The operator only runs migrations

The operator creates Jobs and records their outcome; it never changes the Deployment's replica count or pod template. The chart renders `spec.replicas` exactly as in legacy mode (or leaves it to an HPA), so `kubectl scale`, autoscalers and GitOps tools behave the same whether or not the operator is enabled.

Readiness comes from OpenFGA itself: `IsReady()` reports `NOT_SERVING` while the schema revision is below `MinimumSupportedDatastoreSchemaRevision` (4 since v1.3.x). On a **fresh install** the database is empty, so every pod stays `NotReady` until the first migration Job completes, and `helm install --wait` returns once it has. On an **upgrade** the existing schema already meets that minimum, so new pods pass readiness right away and serve on the previous schema while the Job applies the newer migrations. This relies on OpenFGA migrations being backward compatible, which is also what the Helm hook flow has always done: its init container sees the previous release's completed hook Job and lets new pods start before the new hook runs.

**Rejected alternative — let the operator own the replica count:** the chart could omit `spec.replicas` (or render 0) and have the operator scale the Deployment up once the migration succeeds. Testing this showed three problems: switching an existing release to operator mode removes the field, so both Helm's three-way merge and server-side apply reset the Deployment to one replica until the migration finishes; `kubectl scale` and HPAs are overridden by the operator; and the scale-up buys nothing on upgrades, where the readiness check does not hold pods back.

#### Version tracking via ConfigMap

A ConfigMap (`openfga-migration-status`) records the last successfully migrated version. The operator compares this to the Deployment's image tag to determine if migration is needed. This is:
- Simple to inspect (`kubectl get configmap openfga-migration-status -o yaml`)
- Survives operator restarts
- Can be manually deleted to force re-migration (once the previous migration Job has been cleaned up)

#### Separate ServiceAccount for migrations

The chart creates a dedicated `{fullname}-migration` ServiceAccount that the operator uses for migration Jobs. Users can annotate it with cloud IAM roles that grant DDL permissions, while the runtime ServiceAccount retains only CRUD permissions.

#### Migration Job is a regular resource

The Job created by the operator has no Helm hook annotations. It is a standard Kubernetes Job, visible to ArgoCD, FluxCD, and all Kubernetes tooling. It has an owner reference to the OpenFGA Deployment, so it is garbage collected with it.

#### Failure handling

| Failure | Behavior |
|---------|----------|
| Job fails | Operator sets `MigrationFailed` on the Deployment, keeps the failed Job for 60 seconds so its logs can be read, then replaces it. On a fresh database the pods stay `NotReady`; on an upgrade they keep serving on the previous schema. |
| Job pod never starts | A bad secret reference, image pull error or unschedulable pod never fails the Job. Once the Deployment's pod template changes (the fix rolls out), the operator rebuilds a Job whose pod is not running. |
| Job hangs | No deadline by default, like the Helm hook Job. `activeDeadlineSeconds` can be set, but a migration cut off halfway (an index build, a MySQL table rebuild) starts over on the next attempt. |
| Operator crashes | On restart, re-reads the ConfigMap and Job status and resumes. The retry delay is measured from the failed Job's condition, so it survives restarts. |
| Database unreachable | Job fails to connect. After exhausting `backoffLimit` the cycle above repeats until the database becomes available. |

### Sequence Comparison

**Before (Helm hooks):**

```text
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

```text
helm install
  ├── Create ServiceAccount (runtime), ServiceAccount (migrator)
  ├── Create Secret, Service
  ├── Create Deployment (no init containers)
  ├── Create Operator Deployment
  └── [Helm is done — all resources are regular, no hooks]

(The OpenFGA pods start but stay NotReady: the database has no schema yet.)

Operator starts:
  ├── Detects Deployment image version
  ├── No migration status ConfigMap → migration needed
  ├── Creates Job/openfga-migrate (regular Job, no hooks)
  │     └── Uses openfga-migrator ServiceAccount
  │     └── Runs openfga migrate → succeeds
  ├── Creates ConfigMap with migrated version
  └── Pods pass readiness
```

**After (operator-managed, upgrade with new image):**

```text
helm upgrade
  ├── Patches Deployment with new image tag
  ├── Kubernetes starts rolling update
  │     └── New pods (v1.14) pass readiness on the previous schema
  └── [Helm is done]

Operator reconciles:
  ├── Detects image version differs from ConfigMap
  ├── Creates Job/openfga-migrate → runs migration
  └── Updates ConfigMap → "version: v1.14.0"
```

No hooks. No init containers. No `k8s-wait-for`. All resources are regular Kubernetes objects.

### What Changes in the Helm Chart

Nothing is deleted outright — every change is gated on `openfga-operator.enabled` so the legacy flow remains the default for backward compatibility.

**Gated on `openfga-operator.enabled: false` (legacy Helm-hook flow, rendered when the operator is disabled):**

| File/Section | Behavior when operator is enabled |
|--------------|-----------------------------------|
| `templates/job.yaml` | Skipped — operator creates migration Jobs dynamically |
| `templates/rbac.yaml` | Skipped — no init container needs to poll Job status |
| `values.yaml`: `initContainer.*` | Unused — `k8s-wait-for` not deployed |
| `values.yaml`: `datastore.migrationType`, `datastore.waitForMigrations` | Unused — operator always uses a Job and handles ordering |
| `values.yaml`: `migrate.annotations` | Unused — no Helm hooks |
| Deployment migration init containers | Skipped — OpenFGA's readiness check holds pods until the schema is migrated |

**Added (active only when `openfga-operator.enabled: true`):**

| File/Section | Purpose |
|--------------|---------|
| `values.yaml`: `openfga-operator.enabled` | Toggle the operator subchart |
| `values.yaml`: `openfga-operator.migrationJob.*` | Migration Job backoff, deadline, and TTL configuration |
| `values.yaml`: `migration.serviceAccount.*` | Separate ServiceAccount for migration Jobs |
| `templates/serviceaccount.yaml`: second SA | Migration ServiceAccount |
| `charts/openfga-operator/` | Operator subchart (conditional dependency) |

Users on `openfga-operator.enabled: false` (the default) see identical rendered output to the pre-operator chart, so gradual adoption is possible with no forced migration.

## Consequences

### Positive

- **All 6 migration issues resolved** — no Helm hooks means no ArgoCD/FluxCD/`--wait` incompatibility
- **`k8s-wait-for` eliminated** — removes an unmaintained image with CVEs from the supply chain (#132, #144)
- **Least-privilege enforced** — separate ServiceAccounts for migration (DDL) and runtime (CRUD) (#95)
- **Runtime surface area reduced** — when `openfga-operator.enabled: true`, the legacy migration Job, init-container `k8s-wait-for` logic, and job-watching RBAC are skipped from the rendered manifest
- **Migration is observable** — Job is a regular resource visible in all tools; ConfigMap records migration history; operator conditions surface errors
- **Idempotent and crash-safe** — operator can restart at any point and resume correctly

### Negative

- **Operator is a new runtime dependency** — if the operator pod is unavailable, migrations don't run (but existing running pods are unaffected)
- **Two upgrade paths to document** — `openfga-operator.enabled: true` (new) vs `openfga-operator.enabled: false` (legacy)

### Risks

- **Readiness relies on OpenFGA's schema check** — pods on a fresh database are held back only by `MinimumSupportedDatastoreSchemaRevision` in `pkg/storage/sqlcommon/sqlcommon.go`, and upgrades rely on each release working against the previous schema. Both are OpenFGA guarantees the Helm hook flow already depended on.
- **Migrations run as soon as the image changes** — as with the hook Job, nothing drains traffic first. Some migrations, such as MySQL's `008_collate_identifiers` in v1.18.0, block writes while tables are rebuilt; OpenFGA's runbook recommends draining traffic for those, which stays a manual step.
- **ConfigMap as state store** — if the ConfigMap is accidentally deleted, the operator records the version again from the completed Job while it exists, or re-runs the migration once it has been cleaned up (which is safe — `openfga migrate` is idempotent).
