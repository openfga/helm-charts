#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?}"
: "${GITHUB_SHA:?}"
owner=${GITHUB_REPOSITORY%/*}
repo=${GITHUB_REPOSITORY#*/}
registry="oci://ghcr.io/$owner/helm-charts"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/packages" "$work/verify" "$work/index"
pending="$work/pending"
touch "$pending"

# Listing releases explicitly distinguishes a missing release from an API failure.
gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases?per_page=100" > "$work/releases.json"
for chart in charts/*; do
  [[ -f "$chart/Chart.yaml" ]] || continue
  name=$(awk '/^name:/ {print $2}' "$chart/Chart.yaml" | tr -d '"')
  version=$(awk '/^version:/ {print $2}' "$chart/Chart.yaml" | tr -d '"')
  tag="$name-$version"
  pkg="$work/packages/$tag.tgz"
  state=$(jq -r --arg tag "$tag" \
    '[.[][] | select(.tag_name == $tag)] | if length == 0 then "missing" else .[0].draft | tostring end' \
    "$work/releases.json")

  if [[ "$state" == missing ]]; then
    cr package "$chart" --config .github/cr.yaml --package-path "$work/packages"
    gh release create "$tag" "$pkg" "$pkg.prov" \
      --repo "$GITHUB_REPOSITORY" --target "$GITHUB_SHA" \
      --title "$tag" --generate-notes --draft
  else
    # Never repackage a release on retry: preserve its signed archive byte-for-byte.
    gh release download "$tag" --repo "$GITHUB_REPOSITORY" \
      --pattern "$tag.tgz" --pattern "$tag.tgz.prov" --dir "$work/packages"
  fi
  [[ -s "$pkg" && -s "$pkg.prov" ]] || {
    echo "::error::Release $tag is missing its chart archive or provenance"
    exit 1
  }
  if [[ "$state" != false ]]; then
    printf '%s\t%s\t%s\n' "$name" "$version" "$pkg" >> "$pending"
  fi
done

if [[ -s "$pending" ]]; then
  image_repo=$(awk '/^image:/ {image=1; next} image && /repository:/ {print $2; exit}' charts/openfga-operator/values.yaml)
  image_version=$(awk '/^appVersion:/ {print $2}' charts/openfga-operator/Chart.yaml | tr -d '"')
  image="$image_repo:$image_version"
  # The image build runs independently on main; allow it time to finish.
  for attempt in {1..60}; do
    if docker buildx imagetools inspect "$image"; then
      break
    fi
    if [[ "$attempt" == 60 ]]; then
      echo "::error::Operator image $image is unavailable; leaving releases drafted"
      exit 1
    fi
    echo "Waiting for operator image $image (attempt $attempt/60)"
    sleep 10
  done

  while IFS=$'\t' read -r name version pkg; do
    helm push "$pkg" "$registry"
    helm pull "$registry/$name" --version "$version" --destination "$work/verify"
    cmp "$pkg" "$work/verify/$name-$version.tgz"
  done < "$pending"

  # All required artifacts are available before any release is made public.
  while IFS=$'\t' read -r name version pkg; do
    gh release edit "$name-$version" --repo "$GITHUB_REPOSITORY" --draft=false
  done < "$pending"
fi

# Public release assets must exist before the index links to them. Run this even
# with no pending releases so a failed index push can be repaired on retry.
cr index --owner "$owner" --git-repo "$repo" --config .github/cr.yaml \
  --package-path "$work/packages" --index-path "$work/index/index.yaml" --push
