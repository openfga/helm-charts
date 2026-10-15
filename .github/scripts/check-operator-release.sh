#!/usr/bin/env bash
# Fails when operator image or chart inputs changed but the chart versions that
# publish them did not. Usage: check-operator-release.sh <base-ref>, e.g. origin/main.
#
# CI publishes ghcr.io/openfga/openfga-operator:<appVersion> once and never
# overwrites it, and chart-releaser skips chart versions that already exist.
# Release inputs are therefore only published when, in the same PR:
#   1. charts/openfga-operator/Chart.yaml bumps version
#   2. image changes also bump appVersion
#   3. charts/openfga/Chart.yaml bumps version and pins the new operator chart
# Chart.lock has to match too; `helm dependency build` in CI fails if it does not.
set -euo pipefail

base=$1
image_inputs=(operator/cmd operator/internal operator/go.mod operator/go.sum operator/Dockerfile)
image_changed=false
chart_changed=false
if ! git diff --quiet "$base...HEAD" -- "${image_inputs[@]}"; then
  image_changed=true
fi
if ! git diff --quiet "$base...HEAD" -- charts/openfga-operator; then
  chart_changed=true
fi
if [[ "$image_changed" == "false" && "$chart_changed" == "false" ]]; then
  echo "operator release inputs unchanged"
  exit 0
fi

field() { grep "^$1:" "$2" | awk '{print $2}' | tr -d '"'; }
dependency_version() { awk '/name: openfga-operator/{f=1} f && /version:/{print $2; exit}' "$1" | tr -d '"'; }
at_base() { git show "$base:$1" 2>/dev/null; }
fail() { echo "::error file=$1::$2"; exit 1; }

operator_chart=charts/openfga-operator/Chart.yaml
parent_chart=charts/openfga/Chart.yaml
lock_file=charts/openfga/Chart.lock
operator_version=$(field version "$operator_chart")

if at_base "$operator_chart" > /tmp/base-operator-chart.yaml; then
  if [[ "$(field version /tmp/base-operator-chart.yaml)" == "$(field version "$operator_chart")" ]]; then
    fail "$operator_chart" "operator release inputs changed but version is still $(field version "$operator_chart"); bump it so the chart change is published"
  fi
  if [[ "$image_changed" == "true" ]] &&
    [[ "$(field appVersion /tmp/base-operator-chart.yaml)" == "$(field appVersion "$operator_chart")" ]]; then
    fail "$operator_chart" "operator image inputs changed but appVersion is still $(field appVersion "$operator_chart"); bump it so the image change is published"
  fi
else
  echo "operator chart is new in this PR"
fi

at_base "$parent_chart" > /tmp/base-parent-chart.yaml
if [[ "$(field version /tmp/base-parent-chart.yaml)" == "$(field version $parent_chart)" ]]; then
  fail "$parent_chart" "operator release inputs changed but the openfga chart version is still $(field version $parent_chart); bump it so a chart with the new operator is released"
fi
if [[ "$(dependency_version "$parent_chart")" != "$operator_version" ]]; then
  fail "$parent_chart" "openfga chart pins openfga-operator $(dependency_version "$parent_chart") but the operator chart is $operator_version; update the dependency and run helm dependency update charts/openfga"
fi
if [[ "$(dependency_version "$lock_file")" != "$operator_version" ]]; then
  fail "$lock_file" "Chart.lock pins openfga-operator $(dependency_version "$lock_file") but the operator chart is $operator_version; run helm dependency update charts/openfga"
fi
echo "operator release versions are consistent"
