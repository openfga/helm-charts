#!/usr/bin/env bash
# Fails when the operator image changed but the chart versions that publish it
# did not. Usage: check-operator-release.sh <base-ref>, e.g. origin/main.
#
# CI publishes ghcr.io/openfga/openfga-operator:<appVersion> once and never
# overwrites it, and chart-releaser skips chart versions that already exist. A
# change to the image is therefore only released when, in the same PR:
#   1. charts/openfga-operator/Chart.yaml bumps appVersion and version
#   2. charts/openfga/Chart.yaml bumps version and pins the new operator chart
# Chart.lock has to match too; `helm dependency build` in CI fails if it does not.
set -euo pipefail

base=$1
image_inputs=(operator/cmd operator/internal operator/go.mod operator/go.sum operator/Dockerfile)
if git diff --quiet "$base...HEAD" -- "${image_inputs[@]}"; then
  echo "operator image inputs unchanged"
  exit 0
fi

field() { grep "^$1:" "$2" | awk '{print $2}' | tr -d '"'; }
dependency_version() { awk '/name: openfga-operator/{f=1} f && /version:/{print $2; exit}' "$1" | tr -d '"'; }
at_base() { git show "$base:$1" 2>/dev/null; }
fail() { echo "::error file=$1::$2"; exit 1; }

operator_chart=charts/openfga-operator/Chart.yaml
parent_chart=charts/openfga/Chart.yaml

if at_base "$operator_chart" > /tmp/base-operator-chart.yaml; then
  for f in appVersion version; do
    if [[ "$(field $f /tmp/base-operator-chart.yaml)" == "$(field $f $operator_chart)" ]]; then
      fail "$operator_chart" "operator image inputs changed but $f is still $(field $f $operator_chart); bump appVersion and version so the change is published"
    fi
  done
else
  echo "operator chart is new in this PR"
fi

at_base "$parent_chart" > /tmp/base-parent-chart.yaml
if [[ "$(field version /tmp/base-parent-chart.yaml)" == "$(field version $parent_chart)" ]]; then
  fail "$parent_chart" "operator image inputs changed but the openfga chart version is still $(field version $parent_chart); bump it so a chart with the new operator is released"
fi
if [[ "$(dependency_version $parent_chart)" != "$(field version $operator_chart)" ]]; then
  fail "$parent_chart" "openfga chart pins openfga-operator $(dependency_version $parent_chart) but the operator chart is $(field version $operator_chart); update the dependency and run helm dependency update charts/openfga"
fi
echo "operator release versions are consistent"
