# OpenFGA Operator

A Kubernetes operator that manages database migrations for OpenFGA deployments. Instead of relying on Helm hooks and init containers, the operator watches OpenFGA Deployments, detects version changes, and orchestrates migrations as regular Jobs.

This is **Stage 1** of the operator — focused solely on migration orchestration. See [ADR-001](../docs/adr/001-adopt-openfga-operator.md) for the full roadmap.

## How It Works

1. The operator watches Deployments in its configured namespace, which defaults to the operator pod's namespace, labeled `app.kubernetes.io/part-of: openfga` and `app.kubernetes.io/component: authorization-controller`
2. When a version change is detected (comparing the container image tag to the `{name}-migration-status` ConfigMap), the operator:
   - Creates a migration Job running `openfga migrate`
   - Waits for the Job to complete
   - Updates the ConfigMap with the new version
   - Scales the Deployment to the desired replica count (`openfga.dev/desired-replicas`)
3. On failure, a `MigrationFailed` condition is set on the Deployment and the desired replica count is not applied

The operator never scales the Deployment to 0. A pod that starts before the migration completes is held `NotReady` by OpenFGA's readiness gate on `MinimumSupportedDatastoreSchemaRevision`, so it won't serve traffic against an unmigrated schema.

## Prerequisites

- Go 1.26.8+
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
| No database | `tests/values-no-db.yaml` | Permanent failure: operator retries without crashing; the app pod stays NotReady (0/1) |

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

```text
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
| `--watch-namespace` | `""` | Namespace to watch for OpenFGA Deployments. Defaults to the operator pod's own namespace (via `POD_NAMESPACE` env var). The chart binds namespaced RBAC in the configured watch namespace, so the operator may run in a different namespace when needed. |
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
| `openfga.dev/migration-enabled` | Must be `"true"` for the operator to manage migrations. Deployments without this annotation are ignored. Set by the Helm chart when `operator.enabled`, `migration.enabled`, and `datastore.applyMigrations` are all true. |
| `openfga.dev/desired-replicas` | The replica count the operator scales the Deployment to once migration succeeds. Set by the Helm chart. |
| `openfga.dev/migration-service-account` | The ServiceAccount to use for migration Jobs. Defaults to the Deployment's SA. |

## Limitations

- **Migrations key only on the image tag:** The operator compares the container image tag (or digest) to the `{name}-migration-status` ConfigMap. A mutable tag like `latest`, or a tag reused for a new build, is not seen as a change, so the migration is skipped — use immutable tags (e.g. `v1.14.0`) or pin by digest. A migration-needing change that keeps the same image — for example repointing `datastore.uri` at a different or restored database — also won't trigger a Job; the readiness gate holds the new pod `NotReady`, but you must migrate manually (bump the image or delete the status ConfigMap).
- **Migration-specific volumes:** The legacy Helm chart values `migrate.extraVolumes` and `migrate.extraVolumeMounts` have no effect in operator mode. The operator inherits volumes and mounts from the main Deployment pod spec. If you need additional volumes for migrations (e.g., CA bundles or TLS certs), add them to the top-level `extraVolumes` and `extraVolumeMounts` values instead.
- **Single-container migration Job:** The Job runs one container (`openfga migrate`) with the main container's env, volumes, and scheduling. It injects no sidecars or extra init containers, so databases reached through a sidecar proxy (Cloud SQL Auth Proxy, AlloyDB) aren't supported for operator-managed migrations — a proxy that doesn't exit on its own (e.g. an Istio sidecar) would keep the Job pod running and stop the Job from completing. Connect to such databases directly instead.
- **`envFrom` datastore detection:** The memory-datastore check inspects only the explicit `env` entries on the container. If `OPENFGA_DATASTORE_ENGINE` is supplied via `envFrom` (a ConfigMap or Secret), the operator cannot read the value and will attempt a migration Job that a memory datastore does not need. The Helm chart sets this variable inline, so chart-managed installs are unaffected.
- **GitOps and `spec.replicas`:** The operator owns the replica count — it scales to `openfga.dev/desired-replicas` after the migration Job completes (see [ADR-002](../docs/adr/002-operator-managed-migrations.md)). To avoid fighting a GitOps controller over that field, the chart omits `spec.replicas` in operator mode, the same as an HPA-managed Deployment. Argo CD and Flux treat an absent field as unmanaged, so no `ignoreDifferences` or field-ownership patch is needed. A fresh install starts at the Kubernetes default of one replica, held `NotReady` by the readiness gate until the migration finishes, after which the operator scales to the desired count.
