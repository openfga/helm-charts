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

This will deploy a 3-replica deployment of OpenFGA on the Kubernetes cluster using the default configurations for OpenFGA. For more information on the default values, please see the official [OpenFGA documentation](https://openfga.dev/docs/getting-started/setup-openfga#configuring-the-server). The [Parameters](#parameters) section below lists the parameters that can be configured during installation.

> **Tip**: List all releases using `helm list`

## Uninstalling the Chart
To uninstall/delete the `openfga` deployment:

```
$ helm uninstall openfga
```

## Parameters
