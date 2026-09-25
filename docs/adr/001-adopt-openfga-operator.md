# ADR-001: Adopt a Kubernetes Operator for OpenFGA Lifecycle Management

- **Status:** Proposed
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related Issues:** #211, #107, #120, #100, #95, #126, #132, #144

## Context

The OpenFGA Helm chart currently handles all lifecycle concerns — deployment, configuration, database migrations, and secret management — through Helm templates and hooks. This approach works for simple installations but breaks down in several important scenarios:

1. **Database migrations rely on Helm hooks**, which are incompatible with GitOps tools (ArgoCD, FluxCD) and Helm's own `--wait` flag. This is the single biggest pain point for users, accounting for 6 open issues (#211, #107, #120, #100, #95, #126).

2. **Store provisioning, authorization model updates, and tuple management** are runtime operations that happen through the OpenFGA API. There is no declarative, GitOps-native way to manage these. Teams must use imperative scripts, CI pipelines, or manual API calls to set up stores and push models after deployment.

3. **The migration init container** depends on `groundnuty/k8s-wait-for`, an unmaintained image with known CVEs, pinned by mutable tag (#132, #144).

4. **Migration and runtime workloads share a single ServiceAccount**, violating least-privilege when cloud IAM-based database authentication (AWS IRSA, GCP Workload Identity) maps the ServiceAccount directly to a database role (#95).

### Alternatives Considered

**A. Fix migrations within the Helm chart (no operator)**

- Strip Helm hook annotations from the migration Job by default, rendering it as a regular resource.
- Replace `k8s-wait-for` with a shell-based init container that polls the database schema version directly.
- Add a separate ServiceAccount for the migration Job.

*Pros:* Lower complexity, no new component to maintain.
*Cons:* Doesn't solve the ordering problem cleanly — the Job and Deployment are created simultaneously, requiring an init container to gate startup. Still requires an image or script to poll. Doesn't address store/model/tuple lifecycle at all.

**B. Recommend initContainer mode as default**

- Change `datastore.migrationType` default from `"job"` to `"initContainer"`, running migrations inside each pod.

*Pros:* No separate Job, no hooks, no `k8s-wait-for`.
*Cons:* Every pod runs migrations on startup (wasteful). Rolling updates trigger redundant migrations. Crash-loops on migration failure. Still shares ServiceAccount. No path to store lifecycle management.

**C. Build an operator (selected)**

- A Kubernetes operator manages migrations as internal reconciliation logic and exposes CRDs for store, model, and tuple lifecycle.

*Pros:* Solves all migration issues. Enables GitOps-native authorization management. Follows established Kubernetes patterns (CNPG, Strimzi, cert-manager). Separates concerns cleanly.
*Cons:* Significant development and maintenance investment. New component to deploy and monitor. Learning curve for contributors.

**D. External migration tool (e.g., Flyway, golang-migrate)**

- Remove migrations from the chart entirely and document using an external tool.

*Pros:* Simplifies the chart completely.
*Cons:* Shifts complexity to the user. Every user must build their own migration pipeline. No standard approach across the community.

## Decision

We will build an **OpenFGA Kubernetes Operator**. This ADR decides Stage 1 only:

1. **Database migration orchestration** (Stage 1) — replacing Helm hooks, the `k8s-wait-for` init container, and shared ServiceAccount with operator-managed migration Jobs.

2. **Declarative store lifecycle management** (Stages 2-4) — `FGAStore`, `FGAModel`, and `FGATuples` CRDs for GitOps-native authorization configuration. Under consideration; each stage needs its own ADR before implementation.

The operator will be:
- Written in Go using `controller-runtime` / kubebuilder
- Distributed as a Helm subchart dependency of the main OpenFGA chart
- Optional — `openfga-operator.enabled` defaults to `false`, which keeps the existing behavior

Development will follow a staged approach to deliver value incrementally:

| Stage | Scope | Outcome |
|-------|-------|---------|
| 1 | Operator scaffolding + migration handling | All 6 migration issues resolved |
| 2 | `FGAStore` CRD | Declarative store provisioning |
| 3 | `FGAModel` CRD | Declarative authorization model management |
| 4 | `FGATuples` CRD | Declarative tuple management |

## Implementation Status

Stage 1 is implemented alongside this ADR (openfga chart 0.4.0, openfga-operator chart 0.1.0). Stages 2-4 are not implemented.

### Delivered in Stage 1

- Operator Go project under `/operator/`, built with `controller-runtime`
- Operator packaged as a Helm subchart (`charts/openfga-operator/`) and wired into the main chart via a `condition: openfga-operator.enabled` dependency
- `openfga-operator.enabled` values toggle (default `false`) that gates all operator-managed behavior
- Migration reconciler (`migration_controller.go`) that runs migration Jobs when the operator is enabled
- Optional separate migration ServiceAccount with IAM-annotation support (`openfga.migrationServiceAccountName` helper, `migration.serviceAccount.create`)

### Deferred to later stages

- `FGAStore`, `FGAModel`, and `FGATuples` CRDs and their controllers
- Declarative store/model/tuple lifecycle management

### Backward-compatibility path

When `openfga-operator.enabled: false` (the default), the chart still renders the legacy migration path: the Helm-hook migration Job, the `groundnuty/k8s-wait-for` init container, and the job-status RBAC. Making the operator the default and retiring this path is left to a later ADR.

## Consequences

### Positive

- **Resolves the migration issues** (#211, #107, #120, #100, #126) and related dependency issues (#132, #144) on the operator-enabled path; #95 is addressed by the opt-in migration ServiceAccount
- **Removes `k8s-wait-for` from the operator-enabled path** — the unmaintained, CVE-carrying image is no longer used when `openfga-operator.enabled: true`, and would leave the chart entirely if the legacy path is retired
- **Enables GitOps-native authorization management** (planned, Stages 2-4) — stores, models, and tuples will become declarative Kubernetes resources that ArgoCD/FluxCD can sync
- **Enables least-privilege** — `migration.serviceAccount.create` gives migration Jobs (DDL) a ServiceAccount separate from the runtime (CRUD) on the operator-enabled path
- **Path to simplifying the Helm chart** — the migration Job template, init container logic, job-status RBAC, and hook annotations are conditionalized behind `openfga-operator.enabled: false` and could be removed if the legacy path is retired
- **Follows Kubernetes ecosystem conventions** — operators are the standard pattern for managing stateful application lifecycle

### Negative

- **New component to maintain** — the operator is a full Go project with its own release cycle, CI, testing, and CVE surface
- **Increased deployment footprint** — an additional pod running in the cluster (default requests 10m CPU, 64Mi memory)
- **Learning curve** — contributors need to understand controller-runtime patterns to modify the operator
- **CRD management complexity** (applies once Stages 2-4 land) — Helm does not upgrade or delete CRDs; users may need to apply CRD manifests separately on operator upgrades
- **Two code paths** — the chart must maintain both the operator-enabled path and the legacy path

### Neutral

- **Backward compatibility preserved** — `openfga-operator.enabled: false` (the default) keeps the existing Helm-hook behavior unchanged
- **No change for memory-datastore users** — users running with `datastore.engine: memory` are unaffected (no migrations, no operator needed)
