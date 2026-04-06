# ADR-003: Declarative Store Lifecycle Management via CRDs

- **Status:** Proposed
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related ADR:** [ADR-001](001-adopt-openfga-operator.md)

## Context

OpenFGA is an authorization service. After deploying the server, teams must perform several runtime operations to make it usable:

1. **Create a store** — a logical container for authorization data
2. **Write an authorization model** — the DSL that defines types, relations, and permissions
3. **Write tuples** — the relationship data that the model operates on (e.g., "user:anne is owner of document:budget")

Today, these operations happen outside Kubernetes — through the OpenFGA API, CLI (`fga`), or custom scripts in CI pipelines. There is no declarative, Kubernetes-native way to manage them.

This creates several problems:

- **No GitOps for authorization config** — authorization models live in scripts or API calls, not in version-controlled manifests that ArgoCD/FluxCD sync.
- **No drift detection** — if someone modifies a model or tuple via the API, there's no controller to detect and reconcile the change.
- **No cross-team ownership** — each team that uses OpenFGA must build their own tooling to manage stores and models. There's no standard pattern.
- **Manual coordination** — deploying a new version of an application that needs a model change requires coordinating the Helm upgrade with a separate model push.

### Alternatives Considered

**A. CLI wrapper in CI pipelines**

Use the `fga` CLI in a CI/CD step after `helm upgrade` to create stores, push models, and write tuples.

*Pros:* No new Kubernetes components. Works with any CI system.
*Cons:* Imperative, not declarative. No drift detection. Each team builds their own pipeline. Model changes are not atomic with deployments. No visibility in Kubernetes tooling.

**B. Helm post-install hook Job**

Add a Helm hook Job that runs `fga` CLI commands after installation.

*Pros:* Stays within the Helm ecosystem.
*Cons:* Helm hooks are the exact problem we're solving in ADR-002. Same ArgoCD/FluxCD incompatibilities. Hook Jobs are fire-and-forget with no reconciliation.

**C. CRDs managed by the operator (selected)**

Expose `FGAStore`, `FGAModel`, and `FGATuples` as Custom Resource Definitions. The operator watches these resources and reconciles them against the OpenFGA API.

*Pros:* Fully declarative. GitOps-native. Continuous reconciliation. Standard Kubernetes patterns. Teams own their auth config as manifests.
*Cons:* Requires the operator (ADR-001). CRD design and reconciliation logic add development scope. Tuple reconciliation is complex.

## Decision

Introduce three CRDs, built in stages after the migration handling (ADR-002) is complete:

### Stage 2: FGAStore

```yaml
apiVersion: openfga.dev/v1alpha1
kind: FGAStore
metadata:
  name: my-app
  namespace: my-team
spec:
  # Reference to the OpenFGA instance
  openfgaRef:
    url: openfga.openfga-system.svc:8081
    credentialsRef:
      name: openfga-api-credentials    # Secret with API key or client credentials
  # Store display name
  name: "my-app-store"
status:
  storeId: "01HXYZ..."
  ready: true
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-04-06T12:00:00Z"
```

**Controller behavior:**
- On create: call `CreateStore` API, store the returned store ID in `.status.storeId`
- On delete: call `DeleteStore` API (with finalizer to ensure cleanup)
- Idempotent: if a store with the same name exists, adopt it rather than creating a duplicate
- Status: set `Ready` condition when store is confirmed to exist

### Stage 3: FGAModel

```yaml
apiVersion: openfga.dev/v1alpha1
kind: FGAModel
metadata:
  name: my-app-model
  namespace: my-team
spec:
  storeRef:
    name: my-app                        # References an FGAStore in the same namespace
  model: |
    model
      schema 1.1
    type user
    type organization
      relations
        define member: [user]
        define admin: [user]
    type document
      relations
        define reader: [user, organization#member]
        define writer: [user, organization#admin]
        define owner: [user]
status:
  modelId: "01HABC..."
  ready: true
  lastWrittenHash: "sha256:a1b2c3..."   # Hash of the model DSL to detect changes
  conditions:
    - type: Ready
      status: "True"
    - type: InSync
      status: "True"
```

**Controller behavior:**
- On create/update: hash the model DSL. If hash differs from `.status.lastWrittenHash`, call `WriteAuthorizationModel` API
- Store the returned model ID in `.status.modelId`
- Model writes are append-only in OpenFGA (each write creates a new version), so this is safe
- Validation: optionally validate DSL syntax before calling the API (fail-fast with a clear error condition)
- The controller does NOT delete old model versions — OpenFGA retains model history

### Stage 4: FGATuples

```yaml
apiVersion: openfga.dev/v1alpha1
kind: FGATuples
metadata:
  name: my-app-base-tuples
  namespace: my-team
spec:
  storeRef:
    name: my-app
  tuples:
    - user: "user:anne"
      relation: "owner"
      object: "document:budget"
    - user: "team:engineering#member"
      relation: "reader"
      object: "folder:engineering-docs"
    - user: "organization:acme#admin"
      relation: "writer"
      object: "folder:engineering-docs"
status:
  writtenCount: 3
  ready: true
  lastReconciled: "2026-04-06T12:00:00Z"
  conditions:
    - type: Ready
      status: "True"
    - type: InSync
      status: "True"
```

**Controller behavior:**
- Maintain an **ownership model** — the controller tracks which tuples it wrote (via annotations or a status field). It only manages tuples it owns, never deleting tuples written by the application at runtime.
- On reconciliation: diff the desired tuples (from spec) against owned tuples in the store
  - Tuples in spec but not in store → write them
  - Tuples in store (owned) but not in spec → delete them
  - Tuples in store but not owned → leave them alone
- Pagination: handle large tuple sets that exceed API response limits
- Batching: use `Write` API with batch operations to minimize API calls

**Scope limitation:** `FGATuples` is intended for **base/static tuples** — organizational structure, role assignments, resource hierarchies. It is NOT intended to replace application-level tuple writes for dynamic data (e.g., per-request access grants). The ownership model ensures these two concerns don't interfere.

### CRD Design Principles

1. **Namespace-scoped** — all CRDs are namespaced, allowing teams to manage their own stores/models/tuples in their namespace
2. **Reference-based** — `FGAModel` and `FGATuples` reference an `FGAStore` by name, not by store ID. The controller resolves the reference.
3. **Status-driven** — controllers report state via `.status.conditions` following Kubernetes conventions (`Ready`, `InSync`, error conditions)
4. **Finalizers for cleanup** — `FGAStore` uses a finalizer to ensure the store is deleted from OpenFGA when the CR is deleted
5. **Idempotent** — all operations are safe to retry. Re-running reconciliation produces the same result.
6. **`v1alpha1` API version** — signals that the CRD schema may change. We will promote to `v1beta1` and `v1` as the design stabilizes.

## Consequences

### Positive

- **GitOps-native authorization management** — stores, models, and tuples are Kubernetes resources that ArgoCD/FluxCD sync from Git
- **Drift detection and reconciliation** — the operator continuously ensures the actual state matches the declared state
- **Cross-team standardization** — every team uses the same CRDs, eliminating custom scripts and CI hacks
- **Atomic deployments** — a team can include `FGAModel` in their application's Helm chart; model updates deploy alongside code changes
- **Visibility** — `kubectl get fgastores`, `kubectl get fgamodels`, `kubectl describe fgatuples` provide instant visibility into authorization configuration
- **RBAC integration** — Kubernetes RBAC controls who can create/modify stores, models, and tuples per namespace

### Negative

- **Significant development scope** — three controllers, each with its own reconciliation logic, error handling, and tests
- **Tuple reconciliation complexity** — diffing and ownership tracking for tuples is the most complex piece; edge cases around partial failures, pagination, and large tuple sets
- **CRD upgrade burden** — CRD schema changes require careful migration; Helm does not upgrade CRDs automatically
- **API dependency** — the operator must be able to reach the OpenFGA API; network issues or API downtime affect reconciliation
- **Not suitable for all tuple management** — dynamic, application-driven tuples should still be written via the API, not CRDs. Users must understand this boundary.

### Risks

- **FGATuples at scale** — for stores with millions of tuples, the reconciliation diff could be expensive. The ownership model mitigates this (only diff owned tuples), but documentation must clearly state that `FGATuples` is for base/static data, not high-volume dynamic writes.
- **Multi-cluster** — if OpenFGA serves multiple clusters, CRDs in one cluster may conflict with CRDs in another pointing at the same store. This is out of scope for `v1alpha1` but should be considered for future versions.
