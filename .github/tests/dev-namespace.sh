#!/usr/bin/env bash
set -euo pipefail

source_k=(kubectl -n default)
target_k=(kubectl -n aloop)

# Exercise the same seed, backup, and newer on-disk login a daily refresh can leave behind.
digest=$(printf '%s' "$CODEX_AUTH_JSON" | sha256sum | cut -d ' ' -f 1)
"${source_k[@]}" create secret generic aloop-codex-recovery --dry-run=client -o json \
  --from-literal="auth.json=$CODEX_AUTH_JSON" --from-literal="login-digests=$digest" \
  --from-literal="seed-digest=$digest" | "${source_k[@]}" apply --server-side --field-manager=aloop-refresh -f -
newer=$(jq -c '.tokens.access_token = "worker-before-move"' <<< "$CODEX_AUTH_JSON")
printf '%s' "$newer" | "${source_k[@]}" exec -i deployment/aloop -- sh -ec 'cat > "$CODEX_HOME/auth.json"; printf persistent > .hidden'
source_id=$("${source_k[@]}" get pvc aloop-data -o jsonpath='{.metadata.uid}')
backup=$("${source_k[@]}" get secret aloop-codex-recovery -o json | jq -cS .data)

# A second consumer must abort the offline copy and bring the original agent back online.
image=$("${source_k[@]}" get deployment aloop -o jsonpath='{.spec.template.spec.containers[0].image}')
"${source_k[@]}" run mail-writer --image="$image" --restart=Never --overrides='{
  "spec":{"automountServiceAccountToken":false,"volumes":[{"name":"mail","persistentVolumeClaim":{"claimName":"aloop-data"}}],
  "containers":[{"name":"mail-writer","image":"'"$image"'","command":["sleep","600"],"volumeMounts":[{"name":"mail","mountPath":"/mail"}]}]}}'
"${source_k[@]}" wait --for=condition=Ready pod/mail-writer --timeout=2m
if bash deploy.sh aloop > "$RUNNER_TEMP/blocked-move.log" 2>&1; then exit 1; fi
grep -F 'Cannot copy default/aloop-data while mounted by mail-writer' "$RUNNER_TEMP/blocked-move.log"
test "$("${source_k[@]}" get deployment aloop -o jsonpath='{.status.readyReplicas}')" = 1
test "$("${source_k[@]}" get service aloop -o jsonpath='{.spec.ports[0].nodePort}')" = 30086
test -z "$("${target_k[@]}" get deployment aloop --ignore-not-found -o name)"
"${source_k[@]}" delete pod mail-writer --grace-period=1

# Stop at the durable copy marker and resume the rollout in a separate invocation.
bash migrate-from-default.sh
"${target_k[@]}" get pvc aloop-data -o json | jq -e '.metadata.annotations["aloop.kelos.dev/copy-complete"] == "true"' >/dev/null
test "$("${source_k[@]}" get deployment aloop -o jsonpath='{.spec.replicas}')" = 0
test -z "$("${source_k[@]}" get service aloop --ignore-not-found -o name)"
test -z "$("${target_k[@]}" get deployment aloop --ignore-not-found -o name)"

# The cluster-managed password must move even if GitHub supplies only the login.
ALOOP_WEB_PASSWORD= bash deploy.sh aloop
test "$("${target_k[@]}" get svc aloop -o jsonpath='{.spec.ports[0].nodePort}')" = 30086
test "$("${target_k[@]}" get deployment aloop -o jsonpath='{.status.readyReplicas}')" = 1
test "$("${target_k[@]}" get secret aloop-codex-recovery -o json | jq -cS .data)" = "$backup"
test "$("${target_k[@]}" exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json)" = "$newer"
test "$("${target_k[@]}" exec deployment/aloop -- cat .hidden)" = persistent
"${target_k[@]}" exec deployment/aloop -- sh -ec 'test -s inbox.db && test -e Maildir/tmp/persistence-test'
test -z "$("${source_k[@]}" get deployment/aloop service/aloop secret/aloop-credentials secret/aloop-codex-recovery --ignore-not-found -o name)"
test "$("${source_k[@]}" get pvc aloop-data -o jsonpath='{.metadata.uid}')" = "$source_id"
test -z "$(kubectl get pods -A -l "aloop.kelos.dev/copy-source=$source_id" -o name)"

bash deploy.sh aloop
test "$("${target_k[@]}" exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json)" = "$newer"
