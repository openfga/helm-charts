# OpenFGA Operator

A Kubernetes operator that manages database migrations for OpenFGA deployments. Instead of relying on Helm hooks and init containers, the operator watches OpenFGA Deployments, detects image and datastore changes, and orchestrates migrations as regular Jobs.

This is **Stage 1** of the operator — focused solely on migration orchestration. See [ADR-001](../docs/adr/001-adopt-openfga-operator.md) for the full roadmap.

## How It Works

1. The operator watches Deployments in its configured namespace, which defaults to the operator pod's namespace, labeled `app.kubernetes.io/part-of: openfga` and `app.kubernetes.io/component: authorization-controller`
2. When the migration identity changes (the container image tag plus the `openfga.dev/migration-trigger` annotation, compared to the `{name}-migration-status` ConfigMap), the operator:
   - Creates a migration Job running `openfga migrate`, using the Deployment's image, environment, pod scheduling, init containers, and other containers as [native sidecars](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/)
   - Applies migration-specific containers, volumes, mounts, resources, timeout, labels, and annotations from the Deployment annotations rendered by the chart
   - Waits for the Job to complete
   - Records the identity in the ConfigMap and applies `ttlSecondsAfterFinished` so Kubernetes cleans the Job up
3. On failure, a `MigrationFailed` condition is set on the Deployment. The failed Job is kept for 60 seconds so its logs can be inspected, then replaced with a new one.

A running migration is never interrupted. If the image changes again while a Job's pod is running (a rollback, or two upgrades in a row), the operator waits for that Job to finish and then runs the migration for the new image. Aborting a non-transactional step such as Postgres's concurrent index build in migration 006 leaves an invalid index that the next run skips. To abort a migration that is stuck, delete the Job or set `migrationJob.activeDeadlineSeconds`.

The operator never changes the Deployment's replica count or pod template. On a new database, OpenFGA's readiness check (`MinimumSupportedDatastoreSchemaRevision`) keeps pods `NotReady` until the first migration has run. On an upgrade the existing schema already meets that minimum, so new pods serve on it while the Job applies the newer migrations, which is the same behaviour as the Helm hook flow.

## Prerequisites

- Go 1.26.8+
- Docker
- Helm 3.6+
- A Kubernetes cluster (Rancher Desktop, kind, etc.), 1.29 or newer when the OpenFGA pod has sidecars

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

## Releasing

CI publishes `ghcr.io/openfga/openfga-operator:<appVersion>` on the first push to `main` that carries that appVersion and never overwrites it, and chart-releaser likewise skips chart versions that already exist. A change to the operator image (`cmd/`, `internal/`, `go.mod`, `go.sum`, `Dockerfile`) therefore has to bump, in the same PR:

1. `appVersion` and `version` in `charts/openfga-operator/Chart.yaml`
2. the `openfga-operator` dependency version and `version` in `charts/openfga/Chart.yaml`, then `helm dependency update charts/openfga` to refresh `Chart.lock`

`.github/scripts/check-operator-release.sh origin/main` checks the first two files and runs on every PR; `helm dependency build` fails when `Chart.lock` is stale.

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
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to; `0` disables it. The chart passes `0` unless `metrics.enabled` is set, which also declares a `metrics` container port. The endpoint is plain HTTP without authentication. |
| `--health-probe-bind-address` | `:8081` | Address the Kubernetes liveness and readiness probe endpoints bind to. Change only if the default port conflicts. |
| `--backoff-limit` | `3` | Number of times a migration Job's pod can fail before the Job is considered failed. The operator then sets a `MigrationFailed` condition on the Deployment and replaces the Job 60 seconds after it failed. |
| `--active-deadline-seconds` | `0` | Maximum wall-clock seconds a migration Job can run before Kubernetes terminates it. `0` means no deadline. A deadline cuts off long migrations, such as index builds or MySQL table rebuilds on large tables, which then start over on the next attempt. |
| `--ttl-seconds-after-finished` | `300` | Seconds Kubernetes keeps a completed Job and its pod before garbage-collecting them. Failed Jobs do not receive a TTL and remain available for the operator's 60-second retry delay. |

When deployed via the Helm subchart, these are configured through `values.yaml`. See `charts/openfga-operator/values.yaml` for all available options.

## Annotations

The operator reads these annotations from the OpenFGA Deployment:

| Annotation | Description |
|------------|-------------|
| `openfga.dev/migration-enabled` | Must be `"true"` for the operator to manage migrations. Deployments without this annotation are ignored. Set by the Helm chart when `openfga-operator.enabled` and `datastore.applyMigrations` are true and the datastore is Postgres or MySQL. |
| `openfga.dev/container-name` | The OpenFGA container in the pod spec. Defaults to `openfga`. |
| `openfga.dev/migration-trigger` | Any string that is part of the migration identity alongside the image tag; a change runs the migration again. The chart derives it from the datastore settings and `migration.trigger`. |
| `openfga.dev/migration-service-account` | The ServiceAccount to use for migration Jobs. Defaults to the Deployment's SA. |
| `openfga.dev/migration-init-containers` | JSON array of additional init containers for the migration Job. Generated from `migrate.extraInitContainers`. |
| `openfga.dev/migration-sidecars` | JSON array of additional native sidecars for the migration Job. Generated from `migrate.sidecars`. |
| `openfga.dev/migration-volumes` | JSON array of additional volumes for the migration Job. Generated from `migrate.extraVolumes`. |
| `openfga.dev/migration-volume-mounts` | JSON array of additional mounts for the migration container. Generated from `migrate.extraVolumeMounts`. |
| `openfga.dev/migration-resources` | JSON resource requirements for the migration container. Generated from `datastore.migrations.resources`. |
| `openfga.dev/migration-timeout` | `OPENFGA_TIMEOUT` for the migration container. Generated from `migrate.timeout`. |
| `openfga.dev/migration-annotations` | JSON map of non-Helm annotations for the migration Job and pod. Generated from `migrate.annotations`; `helm.sh/*` hook annotations are excluded. |
| `openfga.dev/migration-labels` | JSON map of additional labels for the migration Job and pod. Generated from `migrate.labels`; operator identity labels take precedence. |

## Security

The operator is namespace-scoped. It watches one namespace, runs with a Role rather than a ClusterRole, and never reads Secrets itself: the migration Job gets the OpenFGA container's environment, including `secretKeyRef` entries, and runs as the service account named in `openfga.dev/migration-service-account`, or the OpenFGA pod's service account when that annotation is absent.

The Role the chart creates in the watch namespace:

| Resource | Verbs | Used for |
|----------|-------|----------|
| `apps/deployments` | get, list, watch | Find opted-in OpenFGA Deployments |
| `apps/deployments/status` | patch | Set the `MigrationFailed` condition |
| `batch/jobs` | get, list, watch, create, delete, patch | Run, replace and expire migration Jobs |
| `configmaps` | get, list, watch, create, update | Record the migrated version |
| `coordination.k8s.io/leases` | get, list, watch, create, update | Leader election |
| `events` | create, patch | `MigrationStarted`, `MigrationSucceeded`, `MigrationFailed`, `MigrationJobConflict` |

Ports: `8081` serves `/healthz` and `/readyz` for the kubelet. `8080` serves Prometheus metrics only when `metrics.enabled` is set; it has no authentication, so restrict it with a NetworkPolicy if the namespace is shared.

Images pushed by `.github/workflows/operator.yml` carry an SBOM and build provenance and are signed with cosign keyless. To verify:

```bash
cosign verify ghcr.io/openfga/openfga-operator:<tag> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/openfga/helm-charts/\.github/workflows/operator\.yml@refs/heads/'
```

Report vulnerabilities through the [security policy](https://github.com/openfga/helm-charts/security/policy), not in a public issue.

## Limitations

- **Secret contents are not observable:** The trigger covers the datastore settings the chart renders (engine, database host and name, Secret names and keys) but not the contents of a Secret that changes under the same name; set `migration.trigger` to a new value in that case. Credentials in the URI are never part of the trigger.
- **Mutable image contents are not observable:** Reusing a tag such as `latest` does not change the Deployment's image reference. Use immutable tags or digests, or change `migration.trigger` when deliberately replacing the contents of a mutable tag.
- **Helm hook metadata:** Operator-managed Jobs ignore `helm.sh/*` entries in `migrate.annotations`. Other migration annotations and labels are forwarded, but cannot override the operator's identity labels.
- **Injected sidecars:** Containers injected by a webhook are not part of the Deployment's pod spec and cannot be converted to native sidecars. Disable injection for the migration pod with `migrate.annotations` if the injected container does not exit.
- **Job pod labels:** The migration pod is labelled `app.kubernetes.io/part-of: openfga` and `app.kubernetes.io/component: migration`, not with the OpenFGA Deployment's `app.kubernetes.io/name`/`instance` labels (which would make it a Service endpoint). A NetworkPolicy that allows database egress only for the OpenFGA pods' labels needs a rule for the migration pod too.
- **Same-name resources:** The operator only replaces a `{name}-migrate` Job it created itself or the chart's legacy hook Job, and only trusts a `{name}-migration-status` ConfigMap owned by the Deployment. Anything else with those names blocks the migration with a `MigrationJobConflict` event until it is removed.
- **One namespace per operator:** The operator reconciles every opted-in OpenFGA Deployment in its watch namespace. Operators installed by several releases in one namespace share a leader election lease, so only one of them is active at a time.
