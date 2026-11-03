#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release-charts.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT

gh() {
  case "$1 $2" in
    "api --paginate")
      case "$STATE" in
        missing) printf '[[]]\n' ;;
        draft) printf '[[{"tag_name":"openfga-1.0.0","draft":true},{"tag_name":"openfga-operator-1.0.0","draft":true}]]\n' ;;
        published) printf '[[{"tag_name":"openfga-1.0.0","draft":false},{"tag_name":"openfga-operator-1.0.0","draft":false}]]\n' ;;
        mixed) printf '[[{"tag_name":"openfga-1.0.0","draft":false},{"tag_name":"openfga-operator-1.0.0","draft":true}]]\n' ;;
        api-failure) return 1 ;;
      esac
      ;;
    "release create")
      echo "draft $3" >> "$LOG"
      [[ "$FAIL" != draft ]]
      ;;
    "release download")
      local tag=$3
      if [[ "$FAIL" == download ]]; then return 1; fi
      while [[ "$1" != --dir ]]; do shift; done
      printf 'archive\n' > "$2/$tag.tgz"
      printf 'provenance\n' > "$2/$tag.tgz.prov"
      echo "download $tag" >> "$LOG"
      ;;
    "release edit")
      if [[ "$FAIL" == undraft ]]; then return 1; fi
      echo "undraft $3" >> "$LOG"
      ;;
    *) echo "Unexpected gh invocation: $*" >&2; return 1 ;;
  esac
}

cr() {
  case "$1" in
    package)
      if [[ "$FAIL" == package ]]; then return 1; fi
      local chart=$2 name
      name=${chart##*/}
      while [[ "$1" != --package-path ]]; do shift; done
      printf 'archive\n' > "$2/$name-1.0.0.tgz"
      printf 'provenance\n' > "$2/$name-1.0.0.tgz.prov"
      echo "package $name" >> "$LOG"
      ;;
    index)
      echo index >> "$LOG"
      [[ "$FAIL" != index ]]
      ;;
    *) echo "Unexpected cr invocation: $*" >&2; return 1 ;;
  esac
}

docker() {
  echo image >> "$LOG"
  [[ "$FAIL" != image ]]
}

sleep() { :; }

helm() {
  case "$1" in
    push)
      echo "push ${2##*/}" >> "$LOG"
      if [[ "$FAIL" == push && "$2" == *openfga-operator* ]]; then return 1; fi
      cp "$2" "$REGISTRY/${2##*/}"
      ;;
    pull)
      local name=${2##*/} version=$4 destination=$6
      if [[ "$FAIL" == pull ]]; then return 1; fi
      echo "pull $name" >> "$LOG"
      cp "$REGISTRY/$name-$version.tgz" "$destination/"
      if [[ "$FAIL" == compare ]]; then printf 'corrupt\n' >> "$destination/$name-$version.tgz"; fi
      ;;
    *) echo "Unexpected helm invocation: $*" >&2; return 1 ;;
  esac
}

export -f gh cr docker sleep helm
export GITHUB_REPOSITORY=openfga/helm-charts GITHUB_SHA=test-sha

run_case() {
  local name=$1 state=$2 failure=$3 expected=$4
  local dir="$root/$name" result=0
  mkdir -p "$dir/charts/openfga" "$dir/charts/openfga-operator" "$dir/registry"
  for chart in openfga openfga-operator; do
    printf 'name: %s\nversion: 1.0.0\nappVersion: "1.0.0"\n' "$chart" > "$dir/charts/$chart/Chart.yaml"
  done
  printf 'image:\n  repository: ghcr.io/openfga/openfga-operator\n' > "$dir/charts/openfga-operator/values.yaml"
  export STATE=$state FAIL=$failure LOG="$dir/log" REGISTRY="$dir/registry"
  touch "$LOG"
  (cd "$dir" && bash "$script") > "$dir/output" 2>&1 || result=$?
  if [[ "$expected" == success && "$result" != 0 ]] ||
     [[ "$expected" == failure && "$result" == 0 ]]; then
    cat "$dir/output"
    echo "FAIL: $name (exit $result)" >&2
    exit 1
  fi
  if [[ "$expected" == failure && "$failure" != index ]]; then
    if grep -Eq '^(undraft|index)' "$LOG"; then
      echo "FAIL: $name published after a failure" >&2
      exit 1
    fi
  fi
}

run_case new-release missing none success
# No undraft may precede the final registry read-back.
last_pull=$(grep -n '^pull ' "$LOG" | tail -1 | cut -d: -f1)
first_undraft=$(grep -n '^undraft ' "$LOG" | head -1 | cut -d: -f1)
[[ "$last_pull" -lt "$first_undraft" ]]
[[ "$(tail -1 "$LOG")" == index ]]
[[ "$(grep -c '^draft ' "$LOG")" == 2 ]]

run_case retry-drafts draft none success
! grep -Eq '^(package|draft) ' "$LOG"
[[ "$(grep -c '^download ' "$LOG")" == 2 ]]
[[ "$(grep -c '^undraft ' "$LOG")" == 2 ]]

run_case already-published published none success
! grep -Eq '^(package|draft|image|push|pull|undraft)' "$LOG"
[[ "$(tail -1 "$LOG")" == index ]]

run_case partially-undrafted mixed none success
[[ "$(grep -c '^push ' "$LOG")" == 1 ]]
[[ "$(grep -c '^undraft ' "$LOG")" == 1 ]]
grep -q '^undraft openfga-operator-1.0.0$' "$LOG"

run_case failed-package missing package failure
run_case failed-draft missing draft failure
run_case failed-download draft download failure
run_case failed-push missing push failure
run_case failed-pull missing pull failure
run_case failed-readback missing compare failure
run_case failed-undraft draft undraft failure
run_case missing-image missing image failure
! grep -q '^push ' "$LOG"
run_case api-error api-failure none failure
run_case failed-index draft index failure
[[ "$(grep -c '^undraft ' "$LOG")" == 2 ]]
run_case repair-index published none success
[[ "$(tail -1 "$LOG")" == index ]]

echo "Chart release publication tests passed"
