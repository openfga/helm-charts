# OpenFGA Helm Chart

This is a Helm chart to deploy [OpenFGA](https://github.com/openfga/openfga) - a high performance and flexible authorization/permission engine built for developers and inspired by Google Zanzibar.

## TL;DR

```sh
helm repo add openfga https://openfga.github.io/helm-charts
helm install openfga openfga/openfga
```

## Installing the Chart via Helm Repository

To install the chart with the release name `openfga`:

```sh
helm repo add openfga https://openfga.github.io/helm-charts
helm install openfga openfga/openfga
```

This will deploy a 3-replica deployment of OpenFGA on the Kubernetes cluster using the default configurations for OpenFGA. For more information on the default values, please see the official [OpenFGA documentation](https://openfga.dev/docs/getting-started/setup-openfga/docker#configuring-the-server). The [Chart Parameters](#chart-parameters) section below lists the parameters that can be configured during installation.

> **Tip**: List all releases using `helm list`

## Installing the chart via OCI Image

This chart is also available for installation from the GitHub OCI registry. It requires helm 3.8+.
To pull from the GitHub OCI registry, run:

```sh
helm install openfga -f values.yaml oci://ghcr.io/openfga/helm-charts
```

## Deprecation Notice

> **The bundled Bitnami PostgreSQL and MySQL sub-charts (`postgresql.enabled` / `mysql.enabled`) are deprecated and will be removed in a future release.** These sub-charts rely on the [Bitnami legacy archive repository](https://github.com/bitnami/charts/tree/archive-full-index), which is no longer actively maintained or receiving security updates.
>
> Use the `extraObjects` pattern with official Docker images instead. See the [Postgres dev/test setup](#devtest-only-quick-postgres-setup) and [MySQL dev/test setup](#devtest-only-quick-mysql-setup) sections below for working examples.

## Migrating Off Bundled Database Sub-Charts

The bundled Bitnami PostgreSQL and MySQL sub-charts (`postgresql.enabled` / `mysql.enabled`) are deprecated and will be removed soon. Migrate to a standalone database to continue receiving security updates.

### Database Version Compatibility

The bundled sub-charts ship with:
- **PostgreSQL 15.4** (`bitnamilegacy/postgresql:15.4.0-debian-11-r45`)
- **MySQL 8.0** (`bitnamilegacy/mysql:8.0.32-debian-11-r14`)

When migrating, use a database version that is **equal to or newer** than the version above. OpenFGA supports PostgreSQL 14+ and MySQL 8.0+.

### Migration Guides

Step-by-step guides with complete values files, Bitnami compatibility notes, and verified migration paths:

- **[Migrate PostgreSQL from Bitnami to Official Docker Image](docs/migrate-postgres-from-bitnami.md)** — covers the init container needed for Bitnami's non-standard config file layout
- **[Migrate MySQL from Bitnami to Official Docker Image](docs/migrate-mysql-from-bitnami.md)** — covers the `--datadir` flag needed for Bitnami's non-standard data directory

Both guides follow the same pattern: protect the PV (a prerequisite `kubectl patch` step), disable the sub-chart, deploy the official image via `extraObjects` reusing the existing PVC, and verify data integrity — with a single `helm upgrade` after the PV is protected.

## Customization

If you wish to customize the OpenFGA deployment you may supply parameters such as the ones listed in the [values.yaml](/charts/openfga/values.yaml).

### Installing with Custom Common Labels

You can specify custom labels to insert into resources inline or via Values files:

```sh
helm install openfga openfga/openfga \
  --set-json 'commonLabels={"app.example.com/domain": "example", "app.example.com/system": "permissions"}'
```

```yaml
commonLabels:
  app.example.com/system: permissions
  app.example.com/domain: example
```

### Installing with Postgres

If you have an existing Postgres deployment, connect OpenFGA to it by providing the `datastore.uri` parameter:

```sh
helm install openfga openfga/openfga \
  --set datastore.engine=postgres \
  --set datastore.uri="postgres://postgres:password@postgres.default.svc.cluster.local:5432/openfga?sslmode=disable"
```

#### Dev/Test Only: Quick Postgres Setup

If you do not have an existing Postgres deployment and just need a quick dev/test environment, you can use `extraObjects` to deploy a minimal Postgres instance alongside OpenFGA. **This is not suitable for production** — use a managed database service or an operator like [CloudNativePG](https://cloudnative-pg.io/) instead.

See [ci/postgres-values.yaml](/charts/openfga/ci/postgres-values.yaml) for a complete working example. To use it:

```sh
helm install openfga openfga/openfga -f postgres-values.yaml
```

### Installing with MySQL

If you have an existing MySQL deployment, connect OpenFGA to it by providing the `datastore.uri` parameter:

```sh
helm install openfga openfga/openfga \
  --set datastore.engine=mysql \
  --set datastore.uri="root:password@tcp(mysql.default.svc.cluster.local:3306)/openfga?parseTime=true"
```

#### Dev/Test Only: Quick MySQL Setup

If you do not have an existing MySQL deployment and just need a quick dev/test environment, you can use `extraObjects` to deploy a minimal MySQL instance alongside OpenFGA. **This is not suitable for production** — use a managed database service or a MySQL operator instead.

See [ci/mysql-values.yaml](/charts/openfga/ci/mysql-values.yaml) for a complete working example. To use it:

```sh
helm install openfga openfga/openfga -f mysql-values.yaml
```

### Using an existing secret for Postgres or MySQL

If you have an existing secret with the connection details for Postgres or MySQL, you can reference the secret in the values file. For example, say you have created the following secret for Postgres:

```sh
kubectl create secret generic my-postgres-secret \
  --from-literal=uri="postgres://postgres.postgres:5432/postgres?sslmode=disable" \
  --from-literal=username=postgres --from-literal=password=password
```

You can reference this secret in the values file as follows:

```yaml
datastore:
  engine: postgres
  existingSecret: my-postgres-secret
  secretKeys:
    uriKey: uri
    usernameKey: username
    passwordKey: password
```

You can also mix and match both static config and secret references. When the secret key is defined, the static config will be ignored. The following example shows how to reference the secret for username and password, but provide the URI statically:

```yaml
datastore:
  engine: postgres
  uri: "postgres://postgres.postgres:5432/postgres?sslmode=disable"
  existingSecret: my-postgres-secret
  secretKeys:
    usernameKey: username
    passwordKey: password
```

### Database Migrations

When `datastore.engine` is `postgres` or `mysql` and `datastore.applyMigrations` is `true` (the default), the chart applies the OpenFGA schema before the server starts. `datastore.migrationType` selects how:

- **`job`** (default) — migrations run in a Kubernetes Job that is a Helm hook. The chart picks the hook events from the datastore configuration:
  - **External datastore** — the Job runs as a `pre-install`/`pre-upgrade` hook, so migrations finish before Helm creates or updates the Deployment. This avoids the deadlock and the uninstall stall described below. The chart selects it only when nothing the Job reads is created by the release, which means all of the following hold:
    - the datastore URI comes from an existing Secret (`datastore.uriSecret`, or `datastore.existingSecret` together with `datastore.secretKeys.uriKey`). A static `datastore.uri` does not qualify, because its chart-managed Secret is created after pre-install hooks run;
    - `datastore.username` and `datastore.password` are unset, or `datastore.secretKeys.usernameKey` and `datastore.secretKeys.passwordKey` read them from `datastore.existingSecret` instead of the chart-managed Secret;
    - the bundled `postgresql` and `mysql` subcharts are disabled;
    - `extraObjects` is empty, since those objects may be the database or its Secret.
  - **Otherwise (legacy default)** — the Job keeps the `post-install`/`post-upgrade`/`post-rollback`/`post-delete` hooks. With `datastore.waitForMigrations: true` the Deployment also gets a `wait-for-migration` init container that blocks the server until the Job succeeds. This deadlocks installs that wait for readiness (`--wait`, `--atomic`, Argo CD, Flux), because the Deployment waits for a Job that Helm creates only after the Deployment is ready, and `post-delete` stalls `helm uninstall`. Prefer an external datastore or `initContainer`.
- **`initContainer`** — migrations run as an init container in the OpenFGA pod. Use this when the database ships **in the same release** (a subchart or `extraObjects`), since it starts alongside the database and retries until reachable.

With the `pre-install`/`pre-upgrade` hooks:

- The Secret and the database must exist before the release is installed. The chart cannot see what a parent chart creates, so if a parent chart creates either of them in the same release, keep the legacy hooks (see below) or use `initContainer`.
- The Job cannot use the chart's ServiceAccount, which Helm creates after pre-install hooks. When `serviceAccount.create` is `true` the Job runs as a separate `<fullname>-migrate` ServiceAccount, a hook with the same events and delete policy as the Job and a lower weight. It gets a copy of `serviceAccount.annotations`; if a cloud IAM role is bound to the ServiceAccount by name, bind it to `<fullname>-migrate` too. Like the Job, it is not removed by `helm uninstall`; set `helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded` in `migrate.annotations` to delete both after each successful migration. When `serviceAccount.create` is `false` the Job runs as `serviceAccount.name`, which must already exist.
- The Deployment gets no `wait-for-migration` init container. The hooks already order the migration before the Deployment, and pods that start later do not depend on the Job, so a failed upgrade or a rollback cannot block them.

`migrate.annotations` overrides the chart's choice. A hook key you set replaces the chart's value and the keys you omit keep it, so `helm.sh/hook: pre-install,pre-upgrade` opts in when the chart cannot tell that the datastore is external (for example with unrelated `extraObjects`), and `helm.sh/hook: post-install,post-upgrade,post-rollback,post-delete` keeps the legacy hooks. A key set to `null` is removed, and `migrate.annotations: null` renders the Job as a regular resource without Helm hooks. With `openfga-operator.enabled: true` no hook Job is rendered and none of the above applies; see the next section.

### Running migrations with the operator

By default the chart runs database migrations from a Helm hook Job and gates the OpenFGA pods on it with an init container. Helm hooks are not run by Argo CD and conflict with `helm install --wait` and Flux, so the chart can instead install the [openfga-operator](../openfga-operator), which runs `openfga migrate` as a regular Job whenever the OpenFGA image or migration inputs change:

```yaml
openfga-operator:
  enabled: true

datastore:
  engine: postgres
  uriSecret: my-postgres-secret
```

The operator only runs migrations; replicas, autoscaling and the pod template stay under the chart's control. It records the migrated version in the `<release>-migration-status` ConfigMap and sets a `MigrationFailed` condition on the Deployment if a migration fails. The migration Job is built from the OpenFGA pod spec, so `sidecars` such as a database proxy and `extraInitContainers` run alongside it, and the `migrate.*` values (labels, non-hook annotations such as `sidecar.istio.io/inject: "false"`, extra volumes, init containers, sidecars, timeout) and `datastore.migrations.resources` are applied to it. The Job runs as the OpenFGA service account; set `migration.serviceAccount.create` to give it a dedicated `<release>-migration` one, for example with cloud IAM annotations for DDL permissions. Migrations run when the image tag or the datastore connection settings change, so pin `image.tag` to a release rather than a floating tag; set `migration.trigger` to any new value to run one on demand. See the [operator README](../../operator/README.md) for how it works and its limitations.

## Uninstalling the Chart

To uninstall/delete the `openfga` deployment:

```sh
helm uninstall openfga
```

## Development

If you are developing or building the chart locally and still using the deprecated Bitnami sub-chart dependencies (`postgresql.enabled` / `mysql.enabled`), you need to add the Bitnami legacy archive repository before running `helm dep update`:

```sh
helm repo add bitnami-legacy https://raw.githubusercontent.com/bitnami/charts/archive-full-index/bitnami
helm dep update charts/openfga
```

This is not required if you are using the recommended `extraObjects` pattern.

## Chart Parameters

Take a look at the Chart [values schema reference](https://artifacthub.io/packages/helm/openfga/openfga?modal=values-schema) for more information on the chart values that can be configured. Chart values that are null will default to the server specific default values. For more information on the server defaults please see the [official server configuration documentation](https://openfga.dev/docs/getting-started/setup-openfga/docker#configuring-the-server).
