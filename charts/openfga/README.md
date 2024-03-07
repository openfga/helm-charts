# OpenFGA Helm Chart
This is a Helm chart to deploy [OpenFGA](https://github.com/openfga/openfga) - a high performance and flexible authorization/permission engine built for developers and inspired by Google Zanzibar.

## TL;DR
```
$ helm repo add openfga https://openfga.github.io/helm-charts
$ helm install openfga openfga/openfga
```

## Installing the Chart
To install the chart with the release name `openfga`:

```
$ helm repo add openfga https://openfga.github.io/helm-charts
$ helm install openfga openfga/openfga
```

This will deploy a 3-replica deployment of OpenFGA on the Kubernetes cluster using the default configurations for OpenFGA. For more information on the default values, please see the official [OpenFGA documentation](https://openfga.dev/docs/getting-started/setup-openfga/docker#configuring-the-server). The [Parameters](#parameters) section below lists the parameters that can be configured during installation.

> **Tip**: List all releases using `helm list`



### Installing with Postgres
If you do not already have a Postgres deployment, you can deploy OpenFGA with Postgres with the following command:

```
$ helm install openfga openfga/openfga \
  --set datastore.engine=postgres \
  --set datastore.uri="postgres://postgres:password@openfga-postgresql.default.svc.cluster.local:5432/postgres?sslmode=disable" \
  --set postgresql.enabled=true \
  --set postgresql.auth.postgresPassword=password \
  --set postgresql.auth.database=postgres
```

This will bootstrap a Postgres deployment using the [`bitnami/postgresql`](https://artifacthub.io/packages/helm/bitnami/postgresql) chart and deploy OpenFGA configured in a way to connect to it.

### Installing with MySQL
If you do not already have a MySQL deployment, you can deploy OpenFGA with MySQL with the following command:

```
$ helm install openfga openfga/openfga \
  --set datastore.engine=mysql \
  --set datastore.uri="root:password@tcp(openfga-mysql.default.svc.cluster.local:3306)/mysql?parseTime=true" \
  --set mysql.enabled=true \
  --set mysql.auth.rootPassword=password \
  --set mysql.auth.database=mysql
```

This will bootstrap a MySQL deployment using the [`bitnami/mysql`](https://artifacthub.io/packages/helm/bitnami/mysql) chart and deploy OpenFGA configured in a way to connect to it.

## Uninstalling the Chart
To uninstall/delete the `openfga` deployment:

```
$ helm uninstall openfga
```

## Chart Parameters
Take a look at the Chart [values schema reference](https://artifacthub.io/packages/helm/openfga/openfga?modal=values-schema) for more information on the chart values that can be configured. Chart values that are null will default to the server specific default values. For more information on the server defaults please see the [official server configuration documentation](https://openfga.dev/docs/getting-started/setup-openfga/docker#configuring-the-server).
