#!/usr/bin/env bash
set -euo pipefail

image=${1:?usage: wait-for-operator-image.sh <image> [attempts] [retry-seconds]}
attempts=${2:-120}
retry_seconds=${3:-10}

for ((attempt = 1; attempt <= attempts; attempt++)); do
  if docker buildx imagetools inspect "$image" >/dev/null 2>&1; then
    echo "operator image is available: $image"
    exit 0
  fi
  if ((attempt < attempts)); then
    sleep "$retry_seconds"
  fi
done

echo "::error::operator image was not published: $image"
exit 1
