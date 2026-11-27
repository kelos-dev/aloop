#!/usr/bin/env bash
set -euo pipefail

namespace=${1:?supply the dev namespace}
if [[ -n ${OPENAI_API_KEY:-}${TODOIST_API_TOKEN:-}${ALOOP_WEB_PASSWORD:-} ]]; then
  : "${OPENAI_API_KEY:?set both dev API credentials}"
  : "${TODOIST_API_TOKEN:?set both dev API credentials}"
  umask 077
  directory=$(mktemp -d)
  trap 'rm -rf "$directory"' EXIT
  for name in OPENAI_API_KEY TODOIST_API_TOKEN ALOOP_WEB_PASSWORD; do
    if [[ -n ${!name:-} ]]; then
      printf '%s' "${!name}" > "$directory/$name"
    fi
  done
  kubectl -n "$namespace" create secret generic aloop-credentials --from-file="$directory" \
    --dry-run=client -o json | kubectl -n "$namespace" apply --server-side --field-manager=aloop-deploy -f -
fi

# Environment variables in a running pod only change when the pod is replaced.
version=$(kubectl -n "$namespace" get secret aloop-credentials -o jsonpath='{.metadata.resourceVersion}')
kustomize edit add patch --patch "apiVersion: apps/v1
kind: Deployment
metadata:
  name: aloop
spec:
  template:
    metadata:
      annotations:
        aloop.kelos.dev/credentials-version: \"$version\""
