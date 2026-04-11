# OpenFGA Operator

A Kubernetes operator that manages database migrations for OpenFGA deployments. Instead of relying on Helm hooks and init containers, the operator watches OpenFGA Deployments, detects version changes, and orchestrates migrations as regular Jobs.

This is **Stage 1** of the operator — focused solely on migration orchestration. See [ADR-001](../docs/adr/001-adopt-openfga-operator.md) for the full roadmap.

## How It Works

1. The operator watches Deployments labeled `app.kubernetes.io/part-of: openfga`
2. When a version change is detected (comparing the container image tag to the `{name}-migration-status` ConfigMap), the operator:
   - Keeps the Deployment at 0 replicas
   - Creates a migration Job running `openfga migrate`
   - Waits for the Job to complete
   - Updates the ConfigMap with the new version
   - Scales the Deployment up to the desired replica count
3. On failure, a `MigrationFailed` condition is set on the Deployment and replicas stay at 0

## Prerequisites

- Go 1.25+
- Docker
- Helm 3.6+
- A Kubernetes cluster (Rancher Desktop, kind, etc.)

## Development

### Build

```bash
cd operator
go build ./...
```

### Test

```bash
go test ./... -v
```

### Lint

```bash
go vet ./...
```

### Docker Image

```bash
docker build -t openfga/openfga-operator:dev .
```

## Local Testing

Integration test values and instructions are in [`tests/`](tests/). Three scenarios are provided:

| Scenario | Values File | What It Tests |
|----------|-------------|---------------|
| Happy path | `tests/values-happy-path.yaml` | Full lifecycle: Postgres up, migration succeeds, OpenFGA scales to 3/3 |
| DB outage & recovery | `tests/values-db-outage.yaml` | Postgres starts at 0 replicas; scale it up later to verify self-healing |
| No database | `tests/values-no-db.yaml` | Permanent failure: operator retries without crashing, app stays at 0 |

Quick start:

```bash
# 1. Build the operator image
cd operator
docker build -t openfga/openfga-operator:dev .

# 2. Update chart dependencies
cd ..
helm dependency update charts/openfga

# 3. Run the happy-path test
kubectl create namespace openfga-test
helm install openfga-test charts/openfga -n openfga-test \
  -f operator/tests/values-happy-path.yaml

# 4. Verify (wait ~30s)
kubectl get all -n openfga-test

# 5. Clean up
helm uninstall openfga-test -n openfga-test
kubectl delete namespace openfga-test
```

See [`tests/README.md`](tests/README.md) for detailed verification steps and all three scenarios.

## Project Structure

```
operator/
├── cmd/
│   └── main.go                          # Entry point, manager setup
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
| `--watch-namespace` | `""` | Namespace to watch for OpenFGA Deployments. Defaults to the operator pod's own namespace (via `POD_NAMESPACE` env var). Set explicitly for multi-namespace setups. |
| `--watch-all-namespaces` | `false` | Watch all namespaces for OpenFGA Deployments, making the operator cluster-wide. Overrides `--watch-namespace`. |
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to. Change only if the default port conflicts with other containers in the pod. |
| `--health-probe-bind-address` | `:8081` | Address the Kubernetes liveness and readiness probe endpoints bind to. Change only if the default port conflicts. |
| `--backoff-limit` | `3` | Number of times a migration Job's pod can fail before the Job is considered failed. After hitting this limit the operator deletes the Job, sets a `MigrationFailed` condition on the Deployment, and retries after a 60-second cooldown. |
| `--active-deadline-seconds` | `300` | Maximum wall-clock seconds a migration Job can run before Kubernetes terminates it. Prevents stuck migrations from blocking the pipeline indefinitely. Increase for very large databases. |
| `--ttl-seconds-after-finished` | `300` | Seconds Kubernetes keeps a completed or failed Job (and its pods) before garbage-collecting them, giving you time to inspect logs. |

When deployed via the Helm subchart, these are configured through `values.yaml`. See `charts/openfga-operator/values.yaml` for all available options.

## Annotations

The operator reads these annotations from the OpenFGA Deployment:

| Annotation | Description |
|------------|-------------|
| `openfga.dev/desired-replicas` | The replica count to restore after migration succeeds. Set by the Helm chart. |
| `openfga.dev/migration-service-account` | The ServiceAccount to use for migration Jobs. Defaults to the Deployment's SA. |
