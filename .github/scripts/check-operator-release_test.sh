#!/usr/bin/env bash
set -euo pipefail

repo=$(git rev-parse --show-toplevel)
check="$repo/.github/scripts/check-operator-release.sh"
failures=0

run_case() {
  local name=$1 expected=$2 image_changed=$3 operator_version=$4 operator_app_version=$5
  local parent_version=$6 parent_dependency=$7 lock_dependency=$8
  local case_dir base result=0

  case_dir=$(mktemp -d)
  mkdir -p "$case_dir/operator/internal/controller" "$case_dir/charts/openfga-operator" "$case_dir/charts/openfga"
  (
    cd "$case_dir"
    git init -q
    git config user.name "Release Guard Test"
    git config user.email "release-guard@example.com"

    printf 'package controller\n' > operator/internal/controller/controller.go
    cat > charts/openfga-operator/Chart.yaml <<'EOF'
apiVersion: v2
name: openfga-operator
version: "1.0.0"
appVersion: "1.0.0"
EOF
    cat > charts/openfga/Chart.yaml <<'EOF'
apiVersion: v2
name: openfga
version: "1.0.0"
dependencies:
  - name: openfga-operator
    version: "1.0.0"
EOF
    cat > charts/openfga/Chart.lock <<'EOF'
dependencies:
- name: openfga-operator
  version: 1.0.0
EOF
    git add .
    git commit -qm base
    base=$(git rev-parse HEAD)

    if [[ "$image_changed" == "true" ]]; then
      printf 'var changed = true\n' >> operator/internal/controller/controller.go
    fi
    cat > charts/openfga-operator/Chart.yaml <<EOF
apiVersion: v2
name: openfga-operator
version: "$operator_version"
appVersion: "$operator_app_version"
EOF
    cat > charts/openfga/Chart.yaml <<EOF
apiVersion: v2
name: openfga
version: "$parent_version"
dependencies:
  - name: openfga-operator
    version: "$parent_dependency"
EOF
    cat > charts/openfga/Chart.lock <<EOF
dependencies:
- name: openfga-operator
  version: $lock_dependency
EOF
    git add .
    if ! git diff --cached --quiet; then
      git commit -qm candidate
    fi

    "$check" "$base" >/dev/null 2>&1 || result=$?
    if [[ "$expected" == "pass" && "$result" -ne 0 ]] ||
       [[ "$expected" == "fail" && "$result" -eq 0 ]]; then
      printf 'FAIL: %s expected %s, exit code %d\n' "$name" "$expected" "$result"
      exit 1
    fi
  ) || failures=$((failures + 1))
}

run_case "unchanged image" pass false 1.0.0 1.0.0 1.0.0 1.0.0 1.0.0
run_case "consistent release" pass true 1.1.0 1.1.0 1.1.0 1.1.0 1.1.0
run_case "operator version unchanged" fail true 1.0.0 1.1.0 1.1.0 1.0.0 1.0.0
run_case "operator appVersion unchanged" fail true 1.1.0 1.0.0 1.1.0 1.1.0 1.1.0
run_case "parent version unchanged" fail true 1.1.0 1.1.0 1.0.0 1.1.0 1.1.0
run_case "parent dependency mismatch" fail true 1.1.0 1.1.0 1.1.0 1.0.0 1.1.0
run_case "lock dependency mismatch" fail true 1.1.0 1.1.0 1.1.0 1.1.0 1.0.0

if [[ "$failures" -ne 0 ]]; then
  printf '%d release guard case(s) failed\n' "$failures"
  exit 1
fi

echo "operator release guard matrix passed"
