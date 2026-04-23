# Getting Started with the OpenFGA Helm Chart

This guide walks you from an empty Kubernetes cluster to your first authorization check using the `openfga` chart. It complements the [chart README](../README.md), which is the configuration reference.

- [Prerequisites](#prerequisites)
- [Step 1 — Choose an install path](#step-1--choose-an-install-path)
- [Step 2 — Install the chart](#step-2--install-the-chart)
- [Step 3 — Verify the install](#step-3--verify-the-install)
- [Step 4 — Make your first authorization check](#step-4--make-your-first-authorization-check)
- [Common production settings](#common-production-settings)
- [Upgrading and uninstalling](#upgrading-and-uninstalling)
- [Troubleshooting](#troubleshooting)
- [Where to go next](#where-to-go-next)

## Prerequisites

- **Kubernetes** 1.23+ cluster and `kubectl` configured for it
- **Helm** 3.8 or newer (3.8+ is required if you install from the OCI registry)
- Permission to create `Deployment`, `Service`, `ServiceAccount`, `Role`, `RoleBinding`, and `Job` resources in the target namespace
- For production paths: a reachable PostgreSQL 14+ or MySQL 8.0+ database

Verify your tools:

```sh
kubectl version
helm version --short
```

## Step 1 — Choose an install path

The chart supports three common starting points. Pick the one that matches your goal.

| Path | Datastore | Persists data? | Use when |
| --- | --- | --- | --- |
| **Try it** | In-memory (`memory`) — chart default | **No** — data is lost on pod restart | Kicking the tires, demos, tutorials |
| **Dev / test** | Ephemeral Postgres or MySQL deployed alongside via `extraObjects` | Only while the dev pod/PVC lives | Local development, CI, reviewing the chart |
| **Production** | External managed PostgreSQL / MySQL wired in via `datastore.uri` + `existingSecret` | Yes | Real workloads |

> **Heads up:** the chart's default `datastore.engine` is `memory`. It is the fastest way to see OpenFGA running, but stores, models, and tuples disappear on every pod restart and across replicas. Pick one of the database paths before you depend on the data.

## Step 2 — Install the chart

Add the Helm repository once:

```sh
helm repo add openfga https://openfga.github.io/helm-charts
helm repo update
```

### Try it (in-memory, default)

```sh
helm install openfga openfga/openfga --namespace openfga --create-namespace
```

This brings up a single OpenFGA pod backed by the in-memory datastore. The chart pins replicas to 1 whenever `datastore.engine` is `memory`, because in-memory data is not shared across pods — `replicaCount` only takes effect with a persistent engine.

### Dev / test with an ephemeral Postgres

A complete, working values file lives at [`ci/postgres-values.yaml`](../ci/postgres-values.yaml). It deploys Postgres via `extraObjects` and wires the chart to it.

Save the file locally (or check out this repo) and install:

```sh
curl -sSLO https://raw.githubusercontent.com/openfga/helm-charts/main/charts/openfga/ci/postgres-values.yaml

helm install openfga openfga/openfga \
  --namespace openfga --create-namespace \
  -f postgres-values.yaml
```

An equivalent example for MySQL lives at [`ci/mysql-values.yaml`](../ci/mysql-values.yaml).

> Not suitable for production: the dev Postgres uses `emptyDir` storage and a static password.

### Production with an external database

Store the connection string and credentials in a secret you control, then point the chart at it:

```sh
kubectl create namespace openfga

kubectl create secret generic openfga-db \
  --namespace openfga \
  --from-literal=uri='postgres://openfga:REPLACE_ME@db.example.internal:5432/openfga?sslmode=require' \
  --from-literal=username=openfga \
  --from-literal=password=REPLACE_ME
```

`prod-values.yaml`:

```yaml
datastore:
  engine: postgres
  existingSecret: openfga-db
  secretKeys:
    uriKey: uri
    usernameKey: username
    passwordKey: password
```

Install:

```sh
helm install openfga openfga/openfga \
  --namespace openfga \
  -f prod-values.yaml
```

A migration `Job` runs as a post-install/post-upgrade hook and applies schema changes before serving traffic. See the [chart README](../README.md#using-an-existing-secret-for-postgres-or-mysql) for MySQL and inline-URI variants.

## Step 3 — Verify the install

```sh
kubectl rollout status deployment/openfga --namespace openfga
kubectl get pods --namespace openfga -l app.kubernetes.io/name=openfga
```

The chart exposes four ports on the pod:

| Port | Purpose |
| --- | --- |
| `8080` | HTTP API (default for the `Service`) |
| `8081` | gRPC API |
| `3000` | Playground UI (enabled by default) |
| `2112` | Prometheus metrics (`/metrics`) |

Port-forward and hit the health endpoint:

```sh
kubectl port-forward --namespace openfga svc/openfga 8080:8080 3000:3000
# In another terminal:
curl -s http://localhost:8080/healthz
# {"status":"SERVING"}
```

Open the Playground at <http://localhost:3000> to browse stores interactively.

## Step 4 — Make your first authorization check

With the port-forward above still running, create a store, write a tiny authorization model, insert a relationship tuple, and run a `check`.

```sh
# 1. Create a store
STORE_ID=$(curl -s -X POST http://localhost:8080/stores \
  -H 'content-type: application/json' \
  -d '{"name":"getting-started"}' | jq -r .id)
echo "Store: $STORE_ID"

# 2. Write an authorization model (document with owner/viewer relations)
MODEL_ID=$(curl -s -X POST "http://localhost:8080/stores/$STORE_ID/authorization-models" \
  -H 'content-type: application/json' \
  -d '{
    "schema_version": "1.1",
    "type_definitions": [
      {"type": "user"},
      {
        "type": "document",
        "relations": {
          "owner":  {"this": {}},
          "viewer": {"this": {}}
        },
        "metadata": {
          "relations": {
            "owner":  {"directly_related_user_types": [{"type": "user"}]},
            "viewer": {"directly_related_user_types": [{"type": "user"}]}
          }
        }
      }
    ]
  }' | jq -r .authorization_model_id)
echo "Model: $MODEL_ID"

# 3. Write a tuple: anne is the owner of document:readme
curl -s -X POST "http://localhost:8080/stores/$STORE_ID/write" \
  -H 'content-type: application/json' \
  -d "{
    \"authorization_model_id\": \"$MODEL_ID\",
    \"writes\": {\"tuple_keys\": [
      {\"user\": \"user:anne\", \"relation\": \"owner\", \"object\": \"document:readme\"}
    ]}
  }"

# 4. Check: is anne the owner?
curl -s -X POST "http://localhost:8080/stores/$STORE_ID/check" \
  -H 'content-type: application/json' \
  -d "{
    \"authorization_model_id\": \"$MODEL_ID\",
    \"tuple_key\": {\"user\": \"user:anne\", \"relation\": \"owner\", \"object\": \"document:readme\"}
  }"
# {"allowed":true, ...}
```

If the final response shows `"allowed": true`, the chart is serving real authorization traffic. You are done.

## Common production settings

A minimal production-minded values file:

```yaml
replicaCount: 3

resources:
  requests:
    cpu: 100m
    memory: 256Mi
  limits:
    memory: 512Mi

datastore:
  engine: postgres
  existingSecret: openfga-db
  secretKeys:
    uriKey: uri
    usernameKey: username
    passwordKey: password

authn:
  method: preshared
  preshared:
    keysSecret: openfga-preshared-keys   # secret you manage; contains the API keys

playground:
  enabled: false                         # disable in production

ingress:
  enabled: true
  className: nginx
  hosts:
    - host: openfga.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: openfga-tls
      hosts:
        - openfga.example.com

telemetry:
  metrics:
    serviceMonitor:
      enabled: true                      # requires Prometheus Operator CRDs

autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
```

See [`values.yaml`](../values.yaml) and the [values schema on Artifact Hub](https://artifacthub.io/packages/helm/openfga/openfga?modal=values-schema) for the full list.

## Upgrading and uninstalling

Upgrade with the same values file:

```sh
helm upgrade openfga openfga/openfga \
  --namespace openfga \
  -f prod-values.yaml
```

The post-upgrade migration `Job` (named `<release>-migrate`) runs to completion before the new pods take traffic. Watch it:

```sh
kubectl get jobs --namespace openfga
kubectl logs --namespace openfga job/openfga-migrate
```

Rollback:

```sh
helm rollback openfga --namespace openfga
```

Uninstall (keeps the database; drop it manually if you need a clean slate):

```sh
helm uninstall openfga --namespace openfga
```

> **Gotcha:** the chart runs its migration `Job` on `post-delete` too, so `helm uninstall` expects the datastore to still be reachable. If you tear down the database first, the uninstall can hang waiting for the hook. Uninstall the chart **before** removing the database, or delete the stuck `openfga-migrate` Job manually (`kubectl delete job openfga-migrate -n openfga`) to unblock it.

## Troubleshooting

| Symptom | Where to look |
| --- | --- |
| Pods stuck in `Init` | `kubectl logs <pod> -c wait-for-migration` — the init container waits for the migration Job to succeed |
| Migration Job fails | `kubectl logs job/openfga-migrate --namespace openfga`; verify `datastore.uri` and credentials |
| `/healthz` returns `NOT_SERVING` | Datastore connectivity; check database reachability and pod events |
| Migrating off bundled Bitnami DB | [migrate-postgres-from-bitnami.md](./migrate-postgres-from-bitnami.md), [migrate-mysql-from-bitnami.md](./migrate-mysql-from-bitnami.md) |

## Where to go next

- [Chart configuration reference](../README.md) — every supported values knob
- [OpenFGA modeling guide](https://openfga.dev/docs/modeling) — design authorization models for your domain
- [OpenFGA API reference](https://openfga.dev/api/service) — full HTTP / gRPC surface
- [OpenFGA community](https://openfga.dev/community) — Slack and discussions
