# OpenFGA Operator

A Kubernetes operator that manages database migrations for OpenFGA deployments. Instead of relying on Helm hooks and init containers, the operator watches OpenFGA Deployments, detects version changes, and orchestrates migrations as regular Jobs.

This Stage 1 implementation focuses solely on migration orchestration. See [ADR-001](../docs/adr/001-adopt-openfga-operator.md) for the full roadmap.

## How It Works

1. The operator watches Deployments **in its own namespace** labeled `app.kubernetes.io/part-of: openfga` and `app.kubernetes.io/component: authorization-controller`
2. When a version change is detected (comparing the container image tag to the `{name}-migration-status` ConfigMap), the operator:
   - Leaves the Deployment's current replica count unchanged
   - Creates a migration Job running `openfga migrate`
   - Waits for the Job to complete
   - Updates the ConfigMap with the new version
   - Restores the desired replica count when the Deployment is still at zero
3. On failure, a `MigrationFailed` condition is set on the Deployment and its replica count remains unchanged

## Prerequisites

- Go 1.26.2+
- Docker
- Helm 3.6+
- A Kubernetes cluster (Rancher Desktop, kind, etc.)

## Development

### Build

```bash
make -C operator build
```

### Test

```bash
make -C operator test
```

### Lint

```bash
make -C operator vet
```

### Docker Image

```bash
docker build -t openfga/openfga-operator:dev operator
```

## Local Testing

The local integration assets cover these scenarios:

- [Happy path](tests/values-happy-path.yaml): PostgreSQL is available for the operator-managed migration.
- [Database outage and recovery](tests/values-db-outage.yaml): PostgreSQL starts at zero replicas so recovery can be exercised after it is scaled up.
- [No database](tests/values-no-db.yaml): the database hostname is unavailable and the operator continues its failure/retry path.

Run this happy-path quick start from the repository root against a kind cluster:

```bash
make -C operator docker-build IMG=openfga/openfga-operator:dev
kind load docker-image openfga/openfga-operator:dev
helm dependency build charts/openfga
kubectl create namespace openfga-test
helm install openfga-test charts/openfga \
  --namespace openfga-test \
  --values operator/tests/values-happy-path.yaml

kubectl get all,configmap,job --namespace openfga-test
kubectl logs deployment/openfga-test-openfga-operator \
  --namespace openfga-test

helm uninstall openfga-test --namespace openfga-test
kubectl delete namespace openfga-test
```

The scenario values use `imagePullPolicy: Never`, so load the local image into kind as shown above. Docker Desktop and Rancher Desktop with the dockerd runtime can use the locally built image directly. See the [local integration test guide](tests/README.md) for scenario-specific verification and recovery steps.

## Project Structure

```
operator/
├── cmd/
│   ├── main.go                          # Entry point, manager setup
│   └── main_test.go                     # Startup validation tests
├── internal/
│   └── controller/
│       ├── migration_controller.go      # Reconciliation loop
│       ├── migration_controller_test.go # Unit tests
│       └── helpers.go                   # Job builder, scaling, ConfigMap helpers
├── Dockerfile                           # Multi-stage build (distroless runtime)
├── Makefile
├── go.mod
└── go.sum
```

## Configuration

The operator accepts the following flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--leader-elect` | `false` | Enable leader election so only one replica actively reconciles at a time. Required when running multiple operator replicas for high availability; standby pods wait for the leader's Lease to expire before taking over. Not needed for single-replica deployments. |
| `--watch-namespace` | `""` | Namespace to watch for OpenFGA Deployments. Defaults to the operator pod's own namespace via `POD_NAMESPACE` or the mounted service-account namespace file. Startup fails rather than widening to cluster scope when no namespace is available. |
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to. Change only if the default port conflicts with other containers in the pod. |
| `--health-probe-bind-address` | `:8081` | Address the Kubernetes liveness and readiness probe endpoints bind to. Change only if the default port conflicts. |
| `--backoff-limit` | `3` | Number of times a migration Job's pod can fail before the Job is considered failed. After hitting this limit the operator deletes the Job, sets a `MigrationFailed` condition on the Deployment, and retries after a 60-second cooldown. |
| `--active-deadline-seconds` | `300` | Maximum wall-clock seconds a migration Job can run before Kubernetes terminates it. Must be at least 1. Prevents stuck migrations from blocking the pipeline indefinitely. |
| `--ttl-seconds-after-finished` | `300` | Seconds Kubernetes keeps a completed or failed Job (and its pods) before garbage-collecting them, giving you time to inspect logs. |

When deployed with the Helm subchart, configure supported flag overrides through [`charts/openfga-operator/values.yaml`](../charts/openfga-operator/values.yaml).

## Annotations

The operator reads these annotations from the OpenFGA Deployment:

| Annotation | Description |
|------------|-------------|
| `openfga.dev/migration-enabled` | Must be `"true"` for the operator to manage migrations. Deployments without this annotation are ignored. |
| `openfga.dev/desired-replicas` | The replica count to restore after migration succeeds when the Deployment is still at zero replicas. |
| `openfga.dev/migration-service-account` | The ServiceAccount to use for migration Jobs. Defaults to the Deployment's SA. |

## Limitations

- **Mutable image tags:** The operator detects version changes by comparing the container image tag (or digest). If you deploy with a mutable tag like `latest` or reuse the same tag for different builds, the operator will not detect changes and will skip the migration. Use immutable tags (e.g., `v1.14.0`) or pin images by digest for reliable migration triggering.
