# ADR-001: Adopt a Kubernetes Operator for OpenFGA Lifecycle Management

- **Status:** Proposed
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related Issues:** #211, #107, #120, #100, #95, #126, #132, #143, #144

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

We will build an **OpenFGA Kubernetes Operator** that handles:

1. **Database migration orchestration** (Stage 1) — replacing Helm hooks, the `k8s-wait-for` init container, and shared ServiceAccount with operator-managed migration Jobs and deployment readiness gating.

2. **Declarative store lifecycle management** (Stages 2-4) — exposing `FGAStore`, `FGAModel`, and `FGATuples` CRDs for GitOps-native authorization configuration.

The operator will be:
- Written in Go using `controller-runtime` / kubebuilder
- Distributed as a Helm subchart dependency of the main OpenFGA chart
- Optional — users who don't need it can set `operator.enabled: false` and fall back to the existing behavior

Development will follow a staged approach to deliver value incrementally:

| Stage | Scope | Outcome |
|-------|-------|---------|
| 1 | Operator scaffolding + migration handling | All 6 migration issues resolved |
| 2 | `FGAStore` CRD | Declarative store provisioning |
| 3 | `FGAModel` CRD | Declarative authorization model management |
| 4 | `FGATuples` CRD | Declarative tuple management |

## Consequences

### Positive

- **Resolves all 6 migration issues** (#211, #107, #120, #100, #95, #126) and related dependency issues (#132, #144)
- **Eliminates `k8s-wait-for` dependency** — removes an unmaintained, CVE-carrying image from the supply chain
- **Enables GitOps-native authorization management** — stores, models, and tuples become declarative Kubernetes resources that ArgoCD/FluxCD can sync
- **Enforces least-privilege** — separate ServiceAccounts for migration (DDL) and runtime (CRUD)
- **Simplifies the Helm chart** — removes migration Job template, init container logic, RBAC for job-status-reading, and hook annotations
- **Follows Kubernetes ecosystem conventions** — operators are the standard pattern for managing stateful application lifecycle

### Negative

- **New component to maintain** — the operator is a full Go project with its own release cycle, CI, testing, and CVE surface
- **Increased deployment footprint** — an additional pod running in the cluster (though resource requirements are minimal: ~50m CPU, ~64Mi memory)
- **Learning curve** — contributors need to understand controller-runtime patterns to modify the operator
- **CRD management complexity** — Helm does not upgrade or delete CRDs; users may need to apply CRD manifests separately on operator upgrades

### Neutral

- **Backward compatibility preserved** — the `operator.enabled: false` fallback maintains the existing Helm hook behavior for users who haven't migrated
- **No change for memory-datastore users** — users running with `datastore.engine: memory` are unaffected (no migrations, no operator needed)
