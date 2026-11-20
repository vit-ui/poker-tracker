#!/usr/bin/env bash
set -euo pipefail

tag=$1
make_latest=$2
shift 2
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$make_latest" == true || "$make_latest" == false ]]
[[ $# -gt 0 ]]
for file in "$@"; do
    test -s "$file"
done

release="$(gh release view "$tag" --json databaseId,isDraft)"
jq -e '.isDraft and .databaseId > 0' <<< "$release" >/dev/null
release_id="$(jq -r '.databaseId' <<< "$release")"
gh release upload "$tag" "$@" --clobber

assets="$(gh api "repos/$GH_REPO/releases/$release_id/assets")"
jq -e --argjson count "$#" 'length == $count' <<< "$assets" >/dev/null
for file in "$@"; do
    digest="$(sha256sum "$file" | cut -d ' ' -f 1)"
    jq -e --arg file "${file##*/}" --arg digest "sha256:$digest" \
        'any(.[]; .name == $file and .state == "uploaded" and .digest == $digest)' <<< "$assets" >/dev/null
done
gh release edit "$tag" --draft=false --latest="$make_latest"
