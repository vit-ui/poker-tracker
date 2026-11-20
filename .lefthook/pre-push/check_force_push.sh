#!/usr/bin/env bash
set -euo pipefail

while read -r local_ref local_oid remote_ref remote_oid; do
    [[ "$remote_ref" == refs/heads/master ]] || continue
    if [[ "$local_ref" == "(delete)" ]]; then
        echo "Refusing to delete master." >&2
        exit 1
    fi
    if [[ "$remote_oid" =~ ^0+$ ]]; then
        continue
    fi
    if ! git merge-base --is-ancestor "$remote_oid" "$local_oid"; then
        echo "Refusing a non-fast-forward update to master. Fetch origin and recheck the branch." >&2
        exit 1
    fi
done
