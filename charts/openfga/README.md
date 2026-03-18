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

## Customization

If you wish to customize the OpenFGA deployment you may supply paremeters such as the ones listed in the [values.yaml](/charts/openfga/values.yaml).

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
