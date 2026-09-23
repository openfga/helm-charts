#!/usr/bin/env bash
set -euo pipefail

repo=$(git rev-parse --show-toplevel)
check="$repo/.github/scripts/wait-for-operator-image.sh"
workflow="$repo/.github/workflows/release.yml"
case_dir=$(mktemp -d)
trap 'rm -rf "$case_dir"' EXIT

cat > "$case_dir/docker" <<'EOF'
#!/usr/bin/env bash
count=$(cat "$DOCKER_CALL_COUNT" 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > "$DOCKER_CALL_COUNT"
[[ "$count" -ge "${DOCKER_SUCCEED_ON:-999}" ]]
EOF
chmod +x "$case_dir/docker"

export PATH="$case_dir:$PATH"
export DOCKER_CALL_COUNT="$case_dir/calls"
export DOCKER_SUCCEED_ON=3
"$check" ghcr.io/openfga/openfga-operator:1.0.0 3 0 >/dev/null
[[ "$(cat "$DOCKER_CALL_COUNT")" == "3" ]]

rm -f "$DOCKER_CALL_COUNT"
export DOCKER_SUCCEED_ON=999
if "$check" ghcr.io/openfga/openfga-operator:1.0.0 2 0 >/dev/null; then
  echo "expected a missing image to fail the release gate"
  exit 1
fi
[[ "$(cat "$DOCKER_CALL_COUNT")" == "2" ]]

wait_line=$(grep -n 'name: Wait for matching operator image' "$workflow" | cut -d: -f1)
release_line=$(grep -n 'name: Run chart-releaser' "$workflow" | cut -d: -f1)
if [[ -z "$wait_line" || -z "$release_line" || "$wait_line" -ge "$release_line" ]]; then
  echo "operator image gate must run before chart-releaser"
  exit 1
fi

echo "operator image release gate passed"
