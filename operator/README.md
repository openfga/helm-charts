# OpenFGA Operator

A Kubernetes operator that manages database migrations for OpenFGA deployments. Instead of relying on Helm hooks and init containers, the operator watches OpenFGA Deployments, detects version changes, and orchestrates migrations as regular Jobs.

This is **Stage 1** of the operator — focused solely on migration orchestration. See [ADR-001](../docs/adr/001-adopt-openfga-operator.md) for the full roadmap.

## How It Works

1. The operator watches Deployments in its configured namespace, which defaults to the operator pod's namespace, labeled `app.kubernetes.io/part-of: openfga` and `app.kubernetes.io/component: authorization-controller`
2. When a version change is detected (comparing the container image tag to the `{name}-migration-status` ConfigMap), the operator:
   - Creates a migration Job running `openfga migrate`
   - Waits for the Job to complete
   - Updates the ConfigMap with the new version
3. On failure, a `MigrationFailed` condition is set on the Deployment. The failed Job is kept for 60 seconds so its logs can be inspected, then replaced with a new one.

The operator never changes the Deployment's replica count or pod template. On a new database, OpenFGA's readiness check (`MinimumSupportedDatastoreSchemaRevision`) keeps pods `NotReady` until the first migration has run. On an upgrade the existing schema already meets that minimum, so new pods serve on it while the Job applies the newer migrations, which is the same behaviour as the Helm hook flow.

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
| Happy path | `tests/values-happy-path.yaml` | Full lifecycle: Postgres up, migration succeeds, OpenFGA ready at 3/3 |
| DB outage & recovery | `tests/values-db-outage.yaml` | Postgres starts at 0 replicas; scale it up later to verify self-healing |
| No database | `tests/values-no-db.yaml` | Permanent failure: operator retries without crashing; the app pods stay NotReady (0/3) |

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
│       └── helpers.go                   # Job builder, status ConfigMap helpers
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
| `--backoff-limit` | `3` | Number of times a migration Job's pod can fail before the Job is considered failed. The operator then sets a `MigrationFailed` condition on the Deployment and replaces the Job 60 seconds after it failed. |
| `--active-deadline-seconds` | `0` | Maximum wall-clock seconds a migration Job can run before Kubernetes terminates it. `0` means no deadline. A deadline cuts off long migrations, such as index builds or MySQL table rebuilds on large tables, which then start over on the next attempt. |
| `--ttl-seconds-after-finished` | `300` | Seconds Kubernetes keeps a completed or failed Job (and its pods) before garbage-collecting them, giving you time to inspect logs. |

When deployed via the Helm subchart, these are configured through `values.yaml`. See `charts/openfga-operator/values.yaml` for all available options.

## Annotations

The operator reads these annotations from the OpenFGA Deployment:

| Annotation | Description |
|------------|-------------|
| `openfga.dev/migration-enabled` | Must be `"true"` for the operator to manage migrations. Deployments without this annotation are ignored. Set by the Helm chart when `openfga-operator.enabled` and `datastore.applyMigrations` are true and the datastore is Postgres or MySQL. |
| `openfga.dev/container-name` | The OpenFGA container in the pod spec. Defaults to `openfga`. |
| `openfga.dev/migration-service-account` | The ServiceAccount to use for migration Jobs. Defaults to the Deployment's SA. |

## Limitations

- **Migrations key only on the image tag:** The operator compares the container image tag (or digest) to the `{name}-migration-status` ConfigMap. A mutable tag like `latest`, or a tag reused for a new build, is not seen as a change, so the migration is skipped — use immutable tags (e.g. `v1.14.0`) or pin by digest. A migration-needing change that keeps the same image — for example repointing `datastore.uri` at a different or restored database — also won't trigger a Job; delete the status ConfigMap (and the `{name}-migrate` Job, if it still exists) to run the migration again.
- **Legacy migration values:** `migrate.*` (extra volumes and mounts, init containers, sidecars, annotations, labels, timeout) and `datastore.migrations.resources` only apply to the legacy Helm hook Job. The operator's Job copies the OpenFGA container's image, env, volumes, resources, security context and scheduling instead, so put anything the migration needs (e.g. CA bundles) in the top-level `extraVolumes`, `extraVolumeMounts` and `extraEnvVars`.
- **Single-container migration Job:** The Job runs one container (`openfga migrate`) and injects no sidecars or extra init containers, so databases reached through a sidecar proxy (Cloud SQL Auth Proxy, AlloyDB) aren't supported for operator-managed migrations. A sidecar injected into every pod in the namespace that doesn't exit on its own (e.g. an Istio sidecar) keeps the Job pod running and stops the Job from completing.
- **Job pod labels:** The migration pod is labelled `app.kubernetes.io/part-of: openfga` and `app.kubernetes.io/component: migration`, not with the OpenFGA Deployment's `app.kubernetes.io/name`/`instance` labels (which would make it a Service endpoint). A NetworkPolicy that allows database egress only for the OpenFGA pods' labels needs a rule for the migration pod too.
- **One namespace per operator:** The operator reconciles every opted-in OpenFGA Deployment in its watch namespace. Operators installed by several releases in one namespace share a leader election lease, so only one of them is active at a time.
