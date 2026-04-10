# Migrating MySQL from Bitnami Sub-Chart to Official Docker Image

This guide walks through migrating from the deprecated bundled Bitnami MySQL sub-chart (`mysql.enabled: true`) to a standalone MySQL instance using official Docker images and the `extraObjects` pattern. All existing data (stores, authorization models, relationship tuples) is preserved.

> **Production deployments** should migrate to a managed database service (e.g., Amazon RDS, Cloud SQL, Azure Database for MySQL) or a MySQL operator. The `extraObjects` approach shown here is suitable for dev/test environments.

## Prerequisites

- Helm 3.x
- `kubectl` access to the cluster
- An existing OpenFGA release using `mysql.enabled: true`

## Overview

| Step | Action |
|------|--------|
| 1 | Protect the existing data volume |
| 2 | Prepare the new values file |
| 3 | Run `helm upgrade` |
| 4 | Verify data integrity |

## Step 1: Protect the Existing Data Volume

The bundled Bitnami sub-chart creates a PersistentVolumeClaim (PVC) named `data-<release>-mysql-0`. When the sub-chart is disabled, Helm will remove the StatefulSet. To prevent the underlying PersistentVolume from being deleted, set its reclaim policy to `Retain`:

```sh
# Find the PVC
kubectl get pvc -n <namespace> -l app.kubernetes.io/instance=<release-name>

# Patch the PV reclaim policy
PV_NAME=$(kubectl get pvc data-<release>-mysql-0 -n <namespace> -o jsonpath='{.spec.volumeName}')
kubectl patch pv "${PV_NAME}" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
```

Confirm the policy change:
```sh
kubectl get pv "${PV_NAME}" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}'
# Should output: Retain
```

## Step 2: Prepare the New Values File

Create a new values file that disables the Bitnami sub-chart and deploys MySQL via `extraObjects`.

### Bitnami Data Directory Compatibility

The Bitnami MySQL image stores data at `/bitnami/mysql/data`. The official `mysql` Docker image defaults to `/var/lib/mysql`.

**Solution:** Mount the existing PVC at `/bitnami/mysql` and pass `--datadir=/bitnami/mysql/data` to the MySQL container so it finds the existing data files in place. Unlike PostgreSQL, no init container is needed — MySQL auto-detects an existing data directory.

### Values File

```yaml
# Disable the bundled Bitnami sub-chart
mysql:
  enabled: false

datastore:
  engine: mysql
  uriSecret: openfga-mysql-credentials
  applyMigrations: true

extraObjects:
  # Connection secret for OpenFGA
  - apiVersion: v1
    kind: Secret
    metadata:
      name: openfga-mysql-credentials
    stringData:
      uri: "<user>:<password>@tcp(openfga-mysql:3306)/<database>?parseTime=true"

  # Standalone MySQL Deployment reusing the existing PVC
  - apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: openfga-mysql
    spec:
      replicas: 1
      selector:
        matchLabels:
          app: openfga-mysql
      template:
        metadata:
          labels:
            app: openfga-mysql
        spec:
          containers:
            - name: mysql
              image: mysql:8.0
              ports:
                - containerPort: 3306
              env:
                - name: MYSQL_ROOT_PASSWORD
                  value: "<password>"
                - name: MYSQL_DATABASE
                  value: "<database>"
              args:
                - --datadir=/bitnami/mysql/data
              volumeMounts:
                - name: data
                  mountPath: /bitnami/mysql
          volumes:
            - name: data
              persistentVolumeClaim:
                claimName: data-<release>-mysql-0

  # Service so OpenFGA can connect
  - apiVersion: v1
    kind: Service
    metadata:
      name: openfga-mysql
    spec:
      selector:
        app: openfga-mysql
      ports:
        - port: 3306
          targetPort: 3306
```

Replace `<release>`, `<user>`, `<password>`, and `<database>` with your actual values.

### Key Details

- **`mysql:8.0`** — matches the Bitnami sub-chart's MySQL 8.0.x. Using the same major version avoids data directory incompatibilities. You can upgrade to a newer version after the migration succeeds.
- **`--datadir=/bitnami/mysql/data`** — tells the official image to use the same data directory path as Bitnami, so the existing data is found in place.
- **No init container needed** — unlike PostgreSQL, MySQL auto-detects an existing data directory and does not require config file fixups.

## Step 3: Run the Upgrade

Delete the previous migration job (Helm cannot update completed Jobs) and upgrade:

```sh
kubectl delete job <release>-migrate -n <namespace> --ignore-not-found
helm upgrade <release> openfga/openfga -n <namespace> -f values.yaml
```

After the upgrade:
- The Bitnami `<release>-mysql-0` StatefulSet pod is removed
- A new `openfga-mysql-*` Deployment pod starts using the same PVC
- The OpenFGA migration job runs and completes
- The OpenFGA app pod connects to the new MySQL instance

## Step 4: Verify Data Integrity

```sh
# Check all pods are healthy
kubectl get pods -n <namespace>

# Confirm OpenFGA is serving
kubectl exec -n <namespace> deploy/<release> -- wget -qO- http://localhost:8080/healthz
# Expected: {"status":"SERVING"}

# Verify your stores exist
kubectl exec -n <namespace> deploy/<release> -- wget -qO- http://localhost:8080/stores

# Spot-check a permission query
curl -s -X POST http://<openfga-host>:8080/stores/<store-id>/check \
  -H "Content-Type: application/json" \
  -d '{"tuple_key":{"user":"user:anne","relation":"owner","object":"document:budget"},"authorization_model_id":"<model-id>"}'
```

## After Migration

Once you have confirmed data integrity, you can optionally reset the PV reclaim policy back to `Delete`:

```sh
kubectl patch pv "${PV_NAME}" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
```

## Tested Migration Path

This migration path has been validated end-to-end on a Kubernetes cluster:

- **From:** Bitnami MySQL sub-chart (`mysql.enabled: true`, MySQL 8.0.32)
- **To:** Official `mysql:8.0` Docker image via `extraObjects`
- **Result:** All stores, authorization models, and relationship tuples preserved. All permission checks passed. Zero data loss, single `helm upgrade` command.
