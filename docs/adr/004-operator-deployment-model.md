# ADR-004: Operator Deployment as Helm Subchart Dependency

- **Status:** Proposed
- **Date:** 2026-04-06
- **Deciders:** OpenFGA Helm Charts maintainers
- **Related ADR:** [ADR-001](001-adopt-openfga-operator.md)

## Context

The OpenFGA Operator (ADR-001) needs a deployment model — how do users install it alongside or independent of the OpenFGA server?

There are several established patterns in the Kubernetes ecosystem:

### Alternatives Considered

**A. Standalone operator chart (install separately)**

Users install the operator chart first, then install the OpenFGA chart. The operator watches for OpenFGA Deployments across namespaces.

*Example:*
```bash
helm install openfga-operator openfga/openfga-operator -n openfga-system
helm install openfga openfga/openfga -n my-namespace
```

*Pros:* Clean separation of concerns. One operator instance serves multiple OpenFGA installations. Follows the OLM/OperatorHub pattern.
*Cons:* Two install steps. Ordering dependency — operator must exist before the chart is useful. Users must manage two releases. Harder to get started.

**B. Operator bundled in the main chart (single chart, always installed)**

The operator Deployment, RBAC, and CRDs are templates in the main OpenFGA chart. No subchart.

*Pros:* Simplest for users — one chart, one install. No dependency management.
*Cons:* Chart becomes larger and harder to maintain. Users who manage the operator separately (e.g., cluster-wide) can't disable it. CRDs are tied to the application chart's release cycle. Multiple OpenFGA installations in the same cluster would deploy multiple operator instances.

**C. Operator as a conditional subchart dependency (selected)**

The operator is a separate Helm chart (`openfga-operator`) that the main chart declares as a conditional dependency. Enabled by default, but users can disable it.

*Example:*
```bash
# Everything in one command
helm install openfga openfga/openfga \
  --set datastore.engine=postgres \
  --set operator.enabled=true

# Or, operator managed separately
helm install openfga-operator openfga/openfga-operator -n openfga-system
helm install openfga openfga/openfga \
  --set operator.enabled=false
```

*Pros:* Single install for most users. Operator chart has its own versioning. Users can disable for standalone management. Clean separation in code.
*Cons:* Subchart dependency adds some Chart.yaml complexity. CRDs still need special handling (Helm's `crds/` directory or a pre-install hook).

**D. OLM (Operator Lifecycle Manager) only**

Publish the operator to OperatorHub. Users install via OLM.

*Pros:* Standard pattern for OpenShift. Handles CRD upgrades, operator upgrades, and RBAC.
*Cons:* OLM is not available on all clusters (not standard on EKS, GKE, AKS). Adds a dependency on OLM itself. Doesn't help Helm-only users.

## Decision

The operator will be distributed as a **conditional Helm subchart dependency** of the main OpenFGA chart.

### Chart Structure

```
helm-charts/
├── charts/
│   ├── openfga/                    # Main chart (existing)
│   │   ├── Chart.yaml              # Declares openfga-operator as dependency
│   │   ├── values.yaml             # operator.enabled: true
│   │   ├── templates/
│   │   └── crds/                   # Empty in Stage 1
│   │
│   └── openfga-operator/           # Operator subchart (new)
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── templates/
│       │   ├── deployment.yaml
│       │   ├── serviceaccount.yaml
│       │   ├── clusterrole.yaml
│       │   └── clusterrolebinding.yaml
│       └── crds/                   # CRDs added in Stages 2-4
│           ├── fgastore.yaml
│           ├── fgamodel.yaml
│           └── fgatuples.yaml
```

### Dependency Declaration

```yaml
# charts/openfga/Chart.yaml
dependencies:
  - name: openfga-operator
    version: "0.1.x"
    repository: "oci://ghcr.io/openfga/helm-charts"
    condition: operator.enabled
```

### CRD Handling

Helm has specific behavior around CRDs:

1. **`crds/` directory** — CRDs placed here are installed on `helm install` but are **never upgraded or deleted** by Helm. This is safe but requires manual CRD upgrades.

2. **Pre-install/pre-upgrade hook Job** — a Job that runs `kubectl apply -f` on CRD manifests before the main install/upgrade. This handles upgrades but reintroduces Helm hooks (the problem ADR-002 solves).

3. **Static manifests applied separately** — CRDs are published as a standalone YAML file. Users run `kubectl apply -f` before `helm install`. This is the pattern used by cert-manager, Istio, and Prometheus Operator.

**Decision:** Use the `crds/` directory in the operator subchart for initial installation. Publish CRD manifests as a standalone artifact for upgrades. Document both paths clearly.

```bash
# First install — Helm installs CRDs automatically
helm install openfga openfga/openfga

# CRD upgrades — applied manually (Helm won't upgrade them)
kubectl apply -f https://github.com/openfga/helm-charts/releases/download/v0.2.0/crds.yaml
```

### Installation Modes

| Mode | Command | Use case |
|------|---------|----------|
| **All-in-one** (default) | `helm install openfga openfga/openfga` | Most users. Single install, operator included. |
| **Operator disabled** | `helm install openfga openfga/openfga --set operator.enabled=false` | Operator managed separately or not needed (memory datastore). |
| **Operator standalone** | `helm install op openfga/openfga-operator -n openfga-system` | Cluster-wide operator serving multiple OpenFGA instances. |

### Multi-Instance Considerations

When multiple OpenFGA installations exist in the same cluster:

- **All-in-one mode:** Each installation gets its own operator instance. The operator only watches resources in its own namespace. This is simple but wasteful.
- **Standalone mode:** One operator installation watches all namespaces (or a configured set). Individual OpenFGA installations set `operator.enabled=false`. This is more efficient for large clusters.

The operator will support both modes via a `watchNamespace` configuration:

```yaml
# Operator values
operator:
  watchNamespace: ""          # empty = watch own namespace only (all-in-one mode)
  # watchNamespace: ""        # or set to a specific namespace
  # watchAllNamespaces: true  # watch all namespaces (standalone mode)
```

## Consequences

### Positive

- **Single `helm install` for most users** — no ordering dependencies, no manual operator setup
- **Opt-out available** — `operator.enabled: false` for users who manage it separately or don't need it
- **Independent versioning** — operator chart has its own version; can be released on a different cadence than the main chart
- **Clean code separation** — operator code and templates are in their own chart directory
- **Standalone installation supported** — cluster admins can install one operator for multiple OpenFGA instances
- **Consistent with ecosystem** — this is the same pattern used by charts that depend on Bitnami PostgreSQL, Redis, etc.

### Negative

- **CRD upgrade complexity** — Helm does not upgrade CRDs; users must apply CRD manifests separately on operator upgrades
- **Multiple operators in all-in-one mode** — if a user installs OpenFGA in three namespaces, they get three operator pods (wasteful). Documentation should recommend standalone mode for multi-instance clusters.
- **Subchart value passing** — configuring the operator requires prefixed values (e.g., `openfga-operator.image.tag`), which is slightly less ergonomic than top-level values

### Neutral

- **OLM support is not excluded** — the operator can be published to OperatorHub in the future alongside the Helm distribution. The two are not mutually exclusive.
