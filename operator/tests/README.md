# Local Integration Tests

Manual integration tests for the OpenFGA operator on a local Kubernetes cluster (Rancher Desktop, kind, minikube, etc.).

## Prerequisites

- A running local Kubernetes cluster
- Helm 3.6+
- The operator image built locally:
  ```bash
  cd operator
  docker build -t openfga/openfga-operator:dev .
  ```
- Chart dependencies updated:
  ```bash
  helm dependency update charts/openfga
  ```

All test values files use `imagePullPolicy: Never`, so the locally-built image must be available to the cluster's container runtime. On Rancher Desktop (dockerd) and Docker Desktop this works automatically. For kind, load the image first:

```bash
kind load docker-image openfga/openfga-operator:dev
```

## Test Scenarios

### 1. Happy Path

Deploys OpenFGA with a Postgres instance. The operator should run the migration and scale OpenFGA up within ~30 seconds.

```bash
kubectl create namespace openfga-test
helm install openfga-test charts/openfga -n openfga-test \
  -f operator/tests/values-happy-path.yaml
```

**Expected outcome:**

| Resource | State |
|----------|-------|
| `openfga-test-openfga-operator` | `1/1 Running` |
| `openfga-test-postgres` | `1/1 Running` |
| `openfga-test-migrate-xxxxx` | `0/1 Completed` |
| `openfga-test` (OpenFGA) | `3/3 Running` |

**Verify:**

```bash
# All resources healthy
kubectl get all -n openfga-test

# Operator logs show full lifecycle
kubectl logs -n openfga-test deployment/openfga-test-openfga-operator

# Migration status recorded
kubectl get configmap openfga-test-migration-status -n openfga-test -o jsonpath='{.data}'

# Database tables created
kubectl exec -n openfga-test deployment/openfga-test-postgres -- \
  psql -U openfga -d openfga -c '\dt'

# OpenFGA responding
kubectl run curl-test --image=curlimages/curl -n openfga-test \
  --rm -it --restart=Never -- curl -s http://openfga-test:8080/healthz
# Expected: {"status":"SERVING"}
```

**Clean up:**

```bash
helm uninstall openfga-test -n openfga-test
kubectl delete namespace openfga-test
```

---

### 2. Database Outage and Recovery

Deploys OpenFGA with a Postgres instance scaled to 0 replicas (simulating a database that isn't ready yet). The operator should retry migrations until Postgres becomes available, then self-heal.

```bash
kubectl create namespace openfga-test
helm install openfga-test charts/openfga -n openfga-test \
  -f operator/tests/values-db-outage.yaml
```

**Expected behavior while Postgres is down:**

- Migration Job runs and fails (each pod times out after ~60s)
- After 3 failures (backoffLimit), the operator:
  - Sets `MigrationFailed: True` condition on the Deployment
  - Deletes the failed Job
  - Creates a fresh Job after a 60-second delay
- This cycle repeats indefinitely
- OpenFGA stays at 0 replicas throughout (safe — no unmigrated app running)

**Watch the failure cycle:**

```bash
# Check deployment conditions
kubectl get deployment openfga-test -n openfga-test \
  -o jsonpath='{range .status.conditions[*]}{.type}: {.status} - {.message}{"\n"}{end}'

# Watch operator logs for delete/retry cycle
kubectl logs -n openfga-test deployment/openfga-test-openfga-operator -f
# Look for:
#   "migration job failed, will delete and retry"
#   "deleted failed migration job, will retry"
#   "created migration job"
```

**Bring Postgres back (after a few minutes):**

```bash
kubectl scale deployment openfga-test-postgres -n openfga-test --replicas=1
```

**Expected recovery (within ~60s of Postgres becoming ready):**

- The currently running migration pod connects and succeeds
- Operator updates the ConfigMap with the new version
- Operator scales OpenFGA to 3/3 replicas
- `{"status":"SERVING"}` from the health endpoint

**Verify recovery:**

```bash
# OpenFGA should be 3/3 Running
kubectl get all -n openfga-test

# Migration status recorded
kubectl get configmap openfga-test-migration-status -n openfga-test -o jsonpath='{.data}'

# Health check
kubectl run curl-test --image=curlimages/curl -n openfga-test \
  --rm -it --restart=Never -- curl -s http://openfga-test:8080/healthz
```

**Clean up:**

```bash
helm uninstall openfga-test -n openfga-test
kubectl delete namespace openfga-test
```

---

### 3. No Database (Permanent Failure)

Deploys OpenFGA pointing at a Postgres hostname that doesn't exist. The operator should continuously retry without crashing or leaving the app in a broken state.

```bash
kubectl create namespace openfga-test
helm install openfga-test charts/openfga -n openfga-test \
  -f operator/tests/values-no-db.yaml
```

**Expected behavior:**

- Migration Jobs fail repeatedly (DNS resolution fails for `postgres-does-not-exist`)
- Operator sets `MigrationFailed: True` on the Deployment
- Operator deletes failed Jobs and retries every ~60 seconds
- OpenFGA stays at 0 replicas indefinitely — never starts against an unmigrated database

This scenario verifies the operator doesn't crash-loop or consume excessive resources when the database is permanently unavailable.

**Verify:**

```bash
# OpenFGA at 0/0, operator at 1/1
kubectl get deployments -n openfga-test

# MigrationFailed condition present
kubectl get deployment openfga-test -n openfga-test \
  -o jsonpath='{range .status.conditions[*]}{.type}: {.status} - {.message}{"\n"}{end}'

# Operator logs show retry cycle
kubectl logs -n openfga-test deployment/openfga-test-openfga-operator --tail=20
```

**Clean up:**

```bash
helm uninstall openfga-test -n openfga-test
kubectl delete namespace openfga-test
```
