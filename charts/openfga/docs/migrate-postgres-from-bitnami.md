# Migrating PostgreSQL from Bitnami Sub-Chart to Official Docker Image

This guide walks through migrating from the deprecated bundled Bitnami PostgreSQL sub-chart (`postgresql.enabled: true`) to a standalone PostgreSQL instance using official Docker images and the `extraObjects` pattern. All existing data (stores, authorization models, relationship tuples) is preserved.

> **Production deployments** should migrate to a managed database service (e.g., Amazon RDS, Cloud SQL, Azure Database for PostgreSQL) or an operator like [CloudNativePG](https://cloudnative-pg.io/). The `extraObjects` approach shown here is suitable for dev/test environments.

## Prerequisites

- Helm 3.x
- `kubectl` access to the cluster
- An existing OpenFGA release using `postgresql.enabled: true`

## Overview

| Step | Action |
|------|--------|
| 1 | Protect the existing data volume |
| 2 | Prepare the new values file |
| 3 | Run `helm upgrade` |
| 4 | Verify data integrity |

## Step 1: Protect the Existing Data Volume

The bundled Bitnami sub-chart creates a PersistentVolumeClaim (PVC) named `data-<release>-postgresql-0`. When the sub-chart is disabled, Helm will remove the StatefulSet. To prevent the underlying PersistentVolume from being deleted, set its reclaim policy to `Retain`:

```sh
# Find the PVC
kubectl get pvc -n <namespace> -l app.kubernetes.io/instance=<release-name>

# Patch the PV reclaim policy
PV_NAME=$(kubectl get pvc data-<release>-postgresql-0 -n <namespace> -o jsonpath='{.spec.volumeName}')
kubectl patch pv "${PV_NAME}" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
```

Confirm the policy change:
```sh
kubectl get pv "${PV_NAME}" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}'
# Should output: Retain
```

## Step 2: Prepare the New Values File

Create a new values file that disables the Bitnami sub-chart and deploys PostgreSQL via `extraObjects`.

### Bitnami Data Directory Compatibility

The Bitnami PostgreSQL image stores data at `/bitnami/postgresql/data` and keeps `postgresql.conf` and `pg_hba.conf` outside the PGDATA directory. The official `postgres` Docker image expects these config files inside PGDATA.

**Solution:** An init container creates the missing config files before the postgres container starts.

### Values File

```yaml
# Disable the bundled Bitnami sub-chart
postgresql:
  enabled: false

datastore:
  engine: postgres
  uriSecret: openfga-postgres-credentials
  applyMigrations: true

extraObjects:
  # Connection secret for OpenFGA
  - apiVersion: v1
    kind: Secret
    metadata:
      name: openfga-postgres-credentials
    stringData:
      uri: "postgres://<user>:<password>@openfga-postgres:5432/<database>?sslmode=disable"

  # Standalone PostgreSQL Deployment reusing the existing PVC
  - apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: openfga-postgres
    spec:
      replicas: 1
      selector:
        matchLabels:
          app: openfga-postgres
      template:
        metadata:
          labels:
            app: openfga-postgres
        spec:
          initContainers:
            - name: fix-bitnami-conf
              image: busybox
              command:
                - sh
                - -c
                - |
                  PGDATA=/data/data
                  if [ ! -f "$PGDATA/postgresql.conf" ]; then
                    cat > "$PGDATA/postgresql.conf" <<'CONF'
                  listen_addresses = '*'
                  max_connections = 100
                  shared_buffers = 128MB
                  dynamic_shared_memory_type = posix
                  max_wal_size = 1GB
                  min_wal_size = 80MB
                  log_timezone = 'UTC'
                  datestyle = 'iso, mdy'
                  timezone = 'UTC'
                  lc_messages = 'en_US.utf8'
                  lc_monetary = 'en_US.utf8'
                  lc_numeric = 'en_US.utf8'
                  lc_time = 'en_US.utf8'
                  default_text_search_config = 'pg_catalog.english'
                  CONF
                    echo "Created postgresql.conf"
                  fi
                  if [ ! -f "$PGDATA/pg_hba.conf" ]; then
                    cat > "$PGDATA/pg_hba.conf" <<'HBA'
                  local   all   all                 trust
                  host    all   all   127.0.0.1/32  scram-sha-256
                  host    all   all   ::1/128       scram-sha-256
                  host    all   all   0.0.0.0/0     scram-sha-256
                  HBA
                    echo "Created pg_hba.conf"
                  fi
              volumeMounts:
                - name: data
                  mountPath: /data
          containers:
            - name: postgres
              image: postgres:15
              ports:
                - containerPort: 5432
              env:
                - name: POSTGRES_USER
                  value: "<user>"
                - name: POSTGRES_PASSWORD
                  value: "<password>"
                - name: PGDATA
                  value: /bitnami/postgresql/data
              volumeMounts:
                - name: data
                  mountPath: /bitnami/postgresql
          volumes:
            - name: data
              persistentVolumeClaim:
                claimName: data-<release>-postgresql-0

  # Service so OpenFGA can connect
  - apiVersion: v1
    kind: Service
    metadata:
      name: openfga-postgres
    spec:
      selector:
        app: openfga-postgres
      ports:
        - port: 5432
          targetPort: 5432
```

Replace `<release>`, `<user>`, `<password>`, and `<database>` with your actual values.

### Key Details

- **`postgres:15`** — matches the Bitnami sub-chart's PostgreSQL 15.4. Using the same major version avoids data directory incompatibilities. You can upgrade to a newer major version after the migration succeeds.
- **`PGDATA=/bitnami/postgresql/data`** — tells the official image to use the same data directory path as Bitnami, so the existing data is found in place.
- **Init container** — creates `postgresql.conf` and `pg_hba.conf` inside PGDATA, which Bitnami stores elsewhere. This only runs once (the `if` guard skips creation on subsequent restarts).

## Step 3: Run the Upgrade

Delete the previous migration job (Helm cannot update completed Jobs) and upgrade:

```sh
kubectl delete job <release>-migrate -n <namespace> --ignore-not-found
helm upgrade <release> openfga/openfga -n <namespace> -f values.yaml
```

After the upgrade:
- The Bitnami `<release>-postgresql-0` StatefulSet pod is removed
- A new `openfga-postgres-*` Deployment pod starts using the same PVC
- The OpenFGA migration job runs and completes
- The OpenFGA app pod connects to the new PostgreSQL instance

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
  -d '{"tuple_key":{"user":"user:alice","relation":"viewer","object":"document:readme"},"authorization_model_id":"<model-id>"}'
```

## After Migration

Once you have confirmed data integrity, you can optionally reset the PV reclaim policy back to `Delete`:

```sh
kubectl patch pv "${PV_NAME}" -p '{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}'
```

## Tested Migration Path

This migration path has been validated end-to-end on a Kubernetes cluster:

- **From:** Bitnami PostgreSQL sub-chart (`postgresql.enabled: true`, PostgreSQL 15.4)
- **To:** Official `postgres:15` Docker image via `extraObjects`
- **Result:** All stores, authorization models, and relationship tuples preserved. All permission checks passed. Zero data loss, single `helm upgrade` command.
