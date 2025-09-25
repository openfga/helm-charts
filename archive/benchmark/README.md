# OpenFGA Benchmark Chart
A Kubernetes Helm chart to deploy OpenFGA+Postgres and run the standard benchmark suite against it.

## Pre-requisites
* k6 Cloud project - if you want to upload the benchmark results to [k6 Cloud](https://k6.io/cloud/) you'll need an account (it's free) and you'll need to set the `k6.projectID` value when installing the chart.

## TL;DR
```
$ helm repo add openfga https://openfga.github.io/helm-charts

$ helm install openfga-benchmark openfga/benchmark \
  --set openfga.replicaCount=1 \
  --set openfga.resources.requests.memory=1Gi \
  --set openfga.resources.requests.cpu=1.0 \
  --set openfga.datastore.engine=postgres \
  --set openfga.datastore.uri="postgres://postgres:password@openfga-benchmark-postgresql.default.svc.cluster.local:5432/postgres?sslmode=disable" \
  --set openfga.postgres.enabled=true \
  --set openfga.postgresql.auth.postgresPassword=password \
  --set openfga.postgresql.auth.database=postgres \
  --set openfga.postgresql.primary.resources.requests.memory=2Gi \
  --set openfga.postgresql.primary.resources.requests.cpu=1.0
```
This will deploy a single replica instance of OpenFGA (with 1vCPU and 1Gi memory) and a Postgres database (with 1vCPU and 2Gi memory) and then proceed to run the OpenFGA benchmark suite against it.

