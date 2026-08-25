# ADR-001: Adopt a Kubernetes Operator for OpenFGA Lifecycle Management

- **Status:** Accepted — Stage 1 implemented
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related Issues:** [#211](https://github.com/openfga/helm-charts/issues/211), [#107](https://github.com/openfga/helm-charts/issues/107), [#120](https://github.com/openfga/helm-charts/issues/120), [#100](https://github.com/openfga/helm-charts/issues/100), [#95](https://github.com/openfga/helm-charts/issues/95), [#126](https://github.com/openfga/helm-charts/issues/126), [#132](https://github.com/openfga/helm-charts/issues/132), [#143](https://github.com/openfga/helm-charts/issues/143), [#144](https://github.com/openfga/helm-charts/issues/144)

## Context

The OpenFGA Helm chart's legacy path handles lifecycle concerns — deployment, configuration, database migrations, and secret management — through Helm templates and hooks. This approach works for simple installations but breaks down in several important scenarios:

1. **Job-based database migrations rely on Helm hooks**, which create GitOps lifecycle and Helm `--wait` problems. The legacy path now uses pre-install/pre-upgrade hooks for eligible external-secret datastores and post-* hooks otherwise, but both remain deploy-time hook orchestration. This is the single biggest pain point for users, accounting for issues [#211](https://github.com/openfga/helm-charts/issues/211), [#107](https://github.com/openfga/helm-charts/issues/107), [#120](https://github.com/openfga/helm-charts/issues/120), [#100](https://github.com/openfga/helm-charts/issues/100), [#95](https://github.com/openfga/helm-charts/issues/95), and [#126](https://github.com/openfga/helm-charts/issues/126).

2. **Store provisioning, authorization model updates, and tuple management** are runtime operations that happen through the OpenFGA API. There is no declarative, GitOps-native way to manage these. Teams must use imperative scripts, CI pipelines, or manual API calls to set up stores and push models after deployment.

3. **The migration init container** depends on `groundnuty/k8s-wait-for`, an inactively maintained image with [reported vulnerabilities](https://github.com/openfga/helm-charts/issues/132). The upstream project has an [open vulnerability report](https://github.com/groundnuty/k8s-wait-for/issues/71) and a [stalled remediation pull request](https://github.com/groundnuty/k8s-wait-for/pull/65), while the chart pins the dependency by mutable tag ([#144](https://github.com/openfga/helm-charts/issues/144)). Related workload security defaults are tracked in [#143](https://github.com/openfga/helm-charts/issues/143).

4. **Legacy post-hook migrations and runtime workloads share a single ServiceAccount**, violating least privilege when cloud IAM-based database authentication (AWS IRSA, GCP Workload Identity) maps the ServiceAccount directly to a database role ([#95](https://github.com/openfga/helm-charts/issues/95)). Eligible external-secret pre-hooks use a dedicated hook-managed migration ServiceAccount, and the operator path uses a separately configurable migration ServiceAccount. The operator Job still inherits datastore `Env` and `EnvFrom`, volumes, volume mounts, and resources from the runtime Deployment, so a separate migration database URI and full migration-specific environment and volume configuration remain future work.

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

*Pros:* Resolves the hook lifecycle and wait problems, and partially addresses [#95](https://github.com/openfga/helm-charts/issues/95) through a separate migration ServiceAccount. Enables GitOps-native authorization management. Follows established Kubernetes patterns (CNPG, Strimzi, cert-manager). Separates concerns cleanly.
*Cons:* Significant development and maintenance investment. New component to deploy and monitor. Learning curve for contributors.

**D. External migration tool (e.g., Flyway, golang-migrate)**

- Remove migrations from the chart entirely and document using an external tool.

*Pros:* Simplifies the chart completely.
*Cons:* Shifts complexity to the user. Every user must build their own migration pipeline. No standard approach across the community.

## Decision

We will build an **OpenFGA Kubernetes Operator** that handles:

1. **Database migration orchestration** (Stage 1), replacing Helm hooks and the `k8s-wait-for` init container on the operator path with operator-managed migration Jobs, a separately configurable migration ServiceAccount, fresh-install replica gating, and OpenFGA's application readiness check during upgrades.

2. **Declarative store lifecycle management** (Stages 2-4) — exposing `FGAStore`, `FGAModel`, and `FGATuples` CRDs for GitOps-native authorization configuration.

The operator will be:
- Written in Go using `controller-runtime` / kubebuilder
- Distributed as a Helm subchart dependency of the main OpenFGA chart
- Optional — users who don't need it can set `operator.enabled: false` and fall back to the existing behavior

Development will follow a staged approach to deliver value incrementally:

| Stage | Scope | Outcome |
|-------|-------|---------|
| 1 | Operator scaffolding + migration handling | [#211](https://github.com/openfga/helm-charts/issues/211), [#107](https://github.com/openfga/helm-charts/issues/107), [#120](https://github.com/openfga/helm-charts/issues/120), [#100](https://github.com/openfga/helm-charts/issues/100), and [#126](https://github.com/openfga/helm-charts/issues/126) resolved on the operator path; [#95](https://github.com/openfga/helm-charts/issues/95) partially addressed through the separate migration ServiceAccount |
| 2 | `FGAStore` CRD | Declarative store provisioning |
| 3 | `FGAModel` CRD | Declarative authorization model management |
| 4 | `FGATuples` CRD | Declarative tuple management |

## Implementation Status

Stage 1 is implemented in [native stack 351](https://github.com/openfga/helm-charts/pull/345) and is pending merge and release. Stages 2-4 are planned but not yet implemented.

### Delivered in Stage 1

- Operator Go project under `operator/`, built with `controller-runtime` and kubebuilder scaffolding
- Operator packaged as a Helm subchart (`charts/openfga-operator/`) and wired into the main chart via a `condition: operator.enabled` dependency
- `operator.enabled` values toggle (default `false`) that gates all operator-managed behavior
- Migration reconciler (`operator/internal/controller/migration_controller.go`) that holds fresh installs at zero replicas until migration succeeds; upgrades preserve the live replica count and rely on OpenFGA's schema-aware readiness endpoint through the configured readiness probe
- Separately configurable operator migration ServiceAccount with IAM-annotation support (`openfga.operatorMigrationServiceAccountName` helper); the chart creates it only when `migration.serviceAccount.create: true`, while `create: false` requires a pre-existing `migration.serviceAccount.name`
- Operator migration Jobs inherit datastore `Env` and `EnvFrom`, volumes, volume mounts, and resources from the runtime Deployment

### Deferred to later stages

- `FGAStore`, `FGAModel`, and `FGATuples` CRDs and their controllers — `charts/openfga-operator/crds/` is reserved but intentionally empty in Stage 1
- Declarative store/model/tuple lifecycle management
- Separate migration database URI and full migration-specific environment and volume configuration

### Release model

The operator uses an intentional monorepo model, not a Git submodule. Controller source lives in `operator/`, the operator chart lives in `charts/openfga-operator/`, and the OpenFGA chart integration lives in `charts/openfga/`. Co-location keeps controller, chart, integration, and end-to-end changes versioned and tested atomically. A separate repository can be reconsidered if ownership or release cadence diverges.

Chart releases run after merge to `main` through `.github/workflows/release.yml` and chart-releaser. Because `CR_SKIP_EXISTING=true`, a pull request with a publishable chart change must bump that chart's `version`. Operator image tags derive from the `appVersion` in `charts/openfga-operator/Chart.yaml`. A releasable runtime change under `operator/` must bump both the operator chart `version` and `appVersion`, although those values do not need to be equal. A packaging-only operator chart change must bump the chart `version` but may leave `appVersion` unchanged.

When the operator chart `version` changes, the pull request must update the operator dependency version in `charts/openfga/Chart.yaml`, regenerate and commit `charts/openfga/Chart.lock`, and bump the parent OpenFGA chart `version` so chart-releaser publishes the dependency update. The pull request guard in `.github/workflows/operator.yml` validates these coordinated bumps and prints remediation; chart-testing lint validates semantic version ordering.

On a `main` push, `.github/workflows/operator.yml` publishes mutable `<appVersion>` and `latest` tags plus immutable `<appVersion>-<sha>` only when `appVersion` changed. If `appVersion` is unchanged, the workflow still builds both platforms but does not overwrite image tags. An explicit workflow dispatch publishes only the immutable tag. Automated versioning such as release-please is intentionally outside this decision and can be reconsidered later.

### Backward-compatibility path (deprecated)

When `operator.enabled: false`, the chart still renders the legacy migration path: the Helm-hook migration Job, the `groundnuty/k8s-wait-for` init container, and the job-status RBAC. Eligible external-secret datastores use pre-install/pre-upgrade hooks; other Job-mode configurations keep the post-* hooks. **This path is deprecated and will be removed in a future release** once the operator is the default and users have had time to migrate. It remains only to preserve backward compatibility during the transition.

## Consequences

### Positive

- **Resolves five migration issues on the operator path:** [#211](https://github.com/openfga/helm-charts/issues/211), [#107](https://github.com/openfga/helm-charts/issues/107), [#120](https://github.com/openfga/helm-charts/issues/120), [#100](https://github.com/openfga/helm-charts/issues/100), and [#126](https://github.com/openfga/helm-charts/issues/126). [#95](https://github.com/openfga/helm-charts/issues/95) is partially addressed by the separate migration ServiceAccount.
- **Removes `k8s-wait-for` from the operator-enabled path:** the inactively maintained image with [reported vulnerabilities](https://github.com/openfga/helm-charts/issues/132) is no longer used when `operator.enabled: true`. The upstream [vulnerability report](https://github.com/groundnuty/k8s-wait-for/issues/71), [stalled remediation](https://github.com/groundnuty/k8s-wait-for/pull/65), and mutable-tag concern ([#144](https://github.com/openfga/helm-charts/issues/144)) remain relevant to the legacy path until it is retired.
- **Enables GitOps-native authorization management** (planned, Stages 2-4) — stores, models, and tuples will become declarative Kubernetes resources that ArgoCD/FluxCD can sync
- **Improves workload identity separation:** the operator-enabled path can use a chart-created or pre-existing migration ServiceAccount for DDL permissions, separate from the runtime account's CRUD permissions. The migration Job still inherits datastore environment, volumes, mounts, and resources from the runtime Deployment, so [#95](https://github.com/openfga/helm-charts/issues/95) remains only partially addressed.
- **Path to simplifying the Helm chart** — the migration Job template, init container logic, job-status RBAC, and hook annotations are conditionalized behind `operator.enabled: false` and scheduled for removal when the legacy path is retired
- **Follows Kubernetes ecosystem conventions** — operators are the standard pattern for managing stateful application lifecycle

### Negative

- **New component to maintain:** the operator is a full Go project with its own image build and publishing lifecycle, CI, testing, and security surface
- **Increased deployment footprint** — an additional pod running in the cluster (though resource requirements are minimal: ~50m CPU, ~64Mi memory)
- **Learning curve** — contributors need to understand controller-runtime patterns to modify the operator
- **CRD management complexity** (applies once Stages 2-4 land) — Helm does not upgrade or delete CRDs; users may need to apply CRD manifests separately on operator upgrades
- **Two code paths during the transition** — the chart must maintain both the operator-enabled path and the deprecated legacy path until the latter is removed

### Neutral

- **Backward compatibility preserved during the transition** — `operator.enabled: false` keeps the existing Helm-hook behavior working for users who have not yet migrated, but this path is deprecated and slated for removal
- **No change for memory-datastore users** — users running with `datastore.engine: memory` are unaffected (no migrations, no operator needed)
