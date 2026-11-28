#!/usr/bin/env bash
set -euo pipefail

source_k=(kubectl -n default)
target_k=(kubectl -n aloop)
uid=$("${source_k[@]}" get pvc aloop-data -o jsonpath='{.metadata.uid}')
: "${uid:?missing source PVC UID}"
helper_label="aloop.kelos.dev/copy-source=$uid"
delete_helpers() {
  local ns
  for ns in default aloop; do
    kubectl -n "$ns" delete pod -l "$helper_label" --timeout=2m >/dev/null || return 1
  done
}
require_idle_claim() {
  local ns=$1 consumers
  consumers=$(kubectl -n "$ns" get pods -o json | jq -r '.items[] |
    select(.status.phase != "Succeeded" and .status.phase != "Failed") |
    select(any(.spec.volumes[]?; .persistentVolumeClaim.claimName == "aloop-data")) | .metadata.name')
  if [[ -n $consumers ]]; then
    echo "Cannot copy $ns/aloop-data while mounted by $consumers" >&2
    return 1
  fi
}
"${source_k[@]}" get deployment aloop -o json | jq -e '
  .spec.selector.matchLabels["app.kubernetes.io/name"] == "aloop" and
  any(.spec.template.spec.volumes[]; .persistentVolumeClaim.claimName == "aloop-data")' >/dev/null
claim=$("${target_k[@]}" get pvc aloop-data --ignore-not-found -o json)
replicas=$("${source_k[@]}" get deployment aloop -o jsonpath='{.spec.replicas}')
port=$("${source_k[@]}" get service aloop --ignore-not-found -o jsonpath='{.spec.ports[0].nodePort}')
if [[ -n $port && $port != 30086 ]]; then
  echo 'The source Service has an unexpected NodePort' >&2
  exit 1
fi

if [[ -z $claim ]]; then
  existing=$("${target_k[@]}" get deployment/aloop service/aloop secret/aloop-credentials secret/aloop-codex-recovery --ignore-not-found -o name)
  if [[ -n $existing ]]; then
    echo 'The target namespace already contains aloop resources' >&2
    exit 1
  fi
  kubectl annotate --local -f data.yaml "aloop.kelos.dev/copy-source=$uid" -o json |
    "${target_k[@]}" create -f -
elif [[ $(jq -r '.metadata.annotations["aloop.kelos.dev/copy-source"]' <<< "$claim") != "$uid" ]]; then
  echo 'The target claim belongs to a different deployment' >&2
  exit 1
fi

copied=$(jq -r '.metadata.annotations["aloop.kelos.dev/copy-complete"] // empty' <<< "$claim")
if [[ $copied == true ]]; then
  if [[ $replicas != 0 ]]; then
    echo 'The source agent has been restarted since its data was copied' >&2
    exit 1
  fi
  delete_helpers
else
  target_deployment=$("${target_k[@]}" get deployment aloop --ignore-not-found -o name)
  if [[ -n $target_deployment || $port != 30086 || ! $replicas =~ ^[01]$ ]]; then
    echo 'Expected a single source agent with NodePort 30086 and an unused destination' >&2
    exit 1
  fi

  copy_secret() {
    local name=$1 manager=$2 key=${3:-} payload
    payload=$("${source_k[@]}" get secret "$name" --ignore-not-found -o json |
      jq -c --arg key "$key" '
        {apiVersion, kind, metadata: {name: .metadata.name}, type,
         data: (if $key == "" then .data else (.data | with_entries(select(.key == $key))) end)}
        | select(.data | length > 0)')
    if [[ -n $payload ]]; then
      # API errors may contain the submitted Secret.
      if ! printf '%s' "$payload" | "${target_k[@]}" apply --server-side --field-manager="$manager" -f - 2>/dev/null; then
        echo "Could not copy Secret $name" >&2
        return 1
      fi
    fi
  }
  copy_secret aloop-credentials aloop-CODEX_AUTH_JSON CODEX_AUTH_JSON
  copy_secret aloop-credentials aloop-ALOOP_WEB_PASSWORD ALOOP_WEB_PASSWORD
  copy_secret aloop-codex-recovery aloop-refresh
  # Check the login before stopping the source, including for cluster-managed credentials.
  "${target_k[@]}" get secret aloop-credentials -o json | jq -e '.data.CODEX_AUTH_JSON | length > 0' >/dev/null

  image=$("${source_k[@]}" get deployment aloop -o jsonpath='{.spec.template.spec.containers[?(@.name=="aloop")].image}')
  : "${image:?missing source agent image}"
  start_helper() {
    local ns=$1 readonly=$2
    kubectl -n "$ns" create -f - -o name <<YAML
apiVersion: v1
kind: Pod
metadata:
  generateName: aloop-copy-
  labels:
    aloop.kelos.dev/copy-source: "$uid"
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  activeDeadlineSeconds: 1800
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true, runAsUser: 1000, fsGroup: 1000, fsGroupChangePolicy: OnRootMismatch}
  containers:
    - name: copy
      image: $image
      command: [sleep, "1800"]
      workingDir: /data
      volumeMounts: [{name: data, mountPath: /data, readOnly: $readonly}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: aloop-data, readOnly: $readonly}
YAML
  }
  restore_source=false
  cleanup() {
    local result=$?
    trap - EXIT
    delete_helpers || result=1
    if [[ $restore_source == true ]]; then
      "${source_k[@]}" scale deployment/aloop --replicas=1
      "${source_k[@]}" rollout status deployment/aloop --timeout=5m
    fi
    exit "$result"
  }
  trap cleanup EXIT
  delete_helpers
  require_idle_claim aloop
  target_pod=$(start_helper aloop false)
  "${target_k[@]}" wait --for=condition=Ready "$target_pod" --timeout=5m
  if [[ $replicas == 1 ]]; then
    restore_source=true
    "${source_k[@]}" scale deployment/aloop --replicas=0
  fi
  "${source_k[@]}" wait --for=delete pod -l app.kubernetes.io/name=aloop --timeout=5m
  # A separate mail delivery pod could still be writing to the claim.
  require_idle_claim default
  source_pod=$(start_helper default true)
  "${source_k[@]}" wait --for=condition=Ready "$source_pod" --timeout=5m

  # The destination is a migration-owned scratch claim until the verified copy is committed.
  "${target_k[@]}" exec "$target_pod" -- find . -mindepth 1 -maxdepth 1 '!' -name lost+found -exec rm -rf -- '{}' +
  archive='find . -mindepth 1 -maxdepth 1 ! -name lost+found -print0 | LC_ALL=C sort -z |
    tar --sort=name --owner=0 --group=0 --numeric-owner --null -cf - -T -'
  "${source_k[@]}" exec "$source_pod" -- bash -euo pipefail -c "$archive" |
    "${target_k[@]}" exec -i "$target_pod" -- tar --no-same-owner -xpf -
  before=$("${source_k[@]}" exec "$source_pod" -- bash -euo pipefail -c "$archive | sha256sum")
  after=$("${target_k[@]}" exec "$target_pod" -- bash -euo pipefail -c "$archive | sha256sum")
  if [[ $before != "$after" ]]; then
    echo 'Data verification failed' >&2
    exit 1
  fi
  "${target_k[@]}" annotate pvc aloop-data aloop.kelos.dev/copy-complete=true
  restore_source=false
  delete_helpers
  trap - EXIT
  echo 'Verified the offline copy of the agent volume'
fi

# A retry after this point rolls forward with the copied state and the source stopped.
if [[ $port == 30086 ]]; then
  "${source_k[@]}" delete service aloop
fi
