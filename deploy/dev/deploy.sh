#!/usr/bin/env bash
set -euo pipefail

namespace=${1:?supply the dev namespace}
migrating=false
namespace_ready=
if [[ $namespace == aloop ]]; then
  if [[ -z $(kubectl get namespace aloop --ignore-not-found -o name) ]]; then
    kubectl create namespace aloop
  fi
  claim=$(kubectl -n aloop get pvc aloop-data --ignore-not-found -o json)
  namespace_ready=$(jq -r '.metadata.annotations["aloop.kelos.dev/namespace-ready"] // empty' <<< "$claim")
  if [[ $namespace_ready != true ]]; then
    original=$(kubectl -n default get deployment aloop --ignore-not-found -o name)
    if [[ -n $original ]]; then
      migrating=true
      bash migrate-from-default.sh
    elif [[ -n $claim ]] && jq -e '.metadata.annotations["aloop.kelos.dev/copy-source"] and
        .metadata.annotations["aloop.kelos.dev/copy-complete"] != "true"' <<< "$claim" >/dev/null; then
      echo 'The source deployment is missing before the data copy completed' >&2
      exit 1
    fi
  fi
fi

kustomize edit set namespace "$namespace"
bash prepare-credentials.sh "$namespace"
kubectl -n "$namespace" apply -k .
kubectl -n "$namespace" rollout status deployment/aloop --timeout=5m

if [[ $migrating == true ]]; then
  # Delete the old Deployment last so an interrupted cleanup resumes on the next rollout.
  config=$(kubectl -n default get deployment aloop -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')
  if [[ $config == aloop-config-* ]]; then
    kubectl -n default delete configmap "$config" --ignore-not-found
  fi
  kubectl -n default delete secret aloop-credentials aloop-codex-recovery --ignore-not-found
  kubectl -n default delete deployment aloop
fi
if [[ $namespace == aloop && $namespace_ready != true ]]; then
  kubectl -n aloop annotate pvc aloop-data aloop.kelos.dev/namespace-ready=true
fi
