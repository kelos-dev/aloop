#!/usr/bin/env bash
set -euo pipefail

namespace=${1:?supply the dev namespace}
umask 077
directory=$(mktemp -d)
trap 'rm -rf "$directory"' EXIT
for name in CODEX_AUTH_JSON ALOOP_WEB_PASSWORD GOOGLE_OAUTH_CLIENT_JSON; do
  if [[ -n ${!name:-} ]]; then
    printf '%s' "${!name}" > "$directory/$name"
    kubectl -n "$namespace" create secret generic aloop-credentials --from-file="$directory/$name" \
      --dry-run=client -o json | kubectl -n "$namespace" apply --server-side --field-manager="aloop-$name" -f -
  fi
done

# Restart the pod so credentials supplied through the environment are reloaded.
version=$(kubectl -n "$namespace" get secret aloop-credentials -o go-template='{{if index .data "CODEX_AUTH_JSON"}}{{.metadata.resourceVersion}}{{end}}')
: "${version:?aloop-credentials must contain CODEX_AUTH_JSON}"
kustomize edit add patch --patch "apiVersion: apps/v1
kind: Deployment
metadata:
  name: aloop
spec:
  template:
    metadata:
      annotations:
        aloop.kelos.dev/credentials-version: \"$version\""
