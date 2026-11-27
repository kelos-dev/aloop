#!/usr/bin/env bash
set -euo pipefail

kubectl exec -i deployment/aloop -- sh -ec 'mkdir /tmp/oauth-test; cat > /tmp/oauth-test/server.cjs' < ../../.github/tests/codex-oauth.cjs
kubectl exec -i deployment/aloop -- sh -es <<'POD'
cat > /tmp/oauth-test/codex <<'SH'
#!/bin/sh
export CODEX_REFRESH_TOKEN_URL_OVERRIDE=http://127.0.0.1:18430
export HTTPS_PROXY=http://127.0.0.1:1 NO_PROXY=127.0.0.1
exec /usr/local/bin/codex "$@"
SH
chmod +x /tmp/oauth-test/codex
node /tmp/oauth-test/server.cjs </dev/null >/tmp/oauth-test/server.log 2>&1 &
POD

# Capture the environment secret outside the cluster and keep Actions credentials out of PR tests.
github_login="$RUNNER_TEMP/github-login"
printf '%s' "$CODEX_AUTH_JSON" > "$github_login"
gh() {
  case "$*" in
    "secret list --repo $GITHUB_REPOSITORY --env dev --json name,updatedAt "*)
      sha256sum "$github_login" | cut -d ' ' -f 1 ;;
    "secret set CODEX_AUTH_JSON --repo $GITHUB_REPOSITORY --env dev")
      [[ -z ${FAIL_PUBLISH:-} ]] || return 1
      cat > "$github_login" ;;
    *) return 2 ;;
  esac
}
export github_login
export -f gh

# A replacement awaiting deployment must survive the scheduled job.
if CODEX_AUTH_JSON='{"tokens":{"access_token":"different"}}' bash refresh-codex-auth.sh default; then exit 1; fi
test -z "$(kubectl get secret aloop-codex-recovery --ignore-not-found -o name)"

# A failed GitHub write must retry with the same bundle.
if FAIL_PUBLISH=1 bash refresh-codex-auth.sh default; then exit 1; fi
rotated=$(kubectl get secret aloop-codex-recovery -o json | jq -r '.data["auth.json"] | @base64d')
jq -e '.tokens.access_token == "fixture-renewed" and .tokens.refresh_token == "fixture-rotated"' <<< "$rotated" >/dev/null
test "$(kubectl exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json)" = "$rotated"
test "$(cat "$github_login")" = "$CODEX_AUTH_JSON"
bash refresh-codex-auth.sh default
test "$(cat "$github_login")" = "$rotated"

# The next scheduled run starts with yesterday's GitHub secret while the original seed is still mounted.
CODEX_AUTH_JSON=$(cat "$github_login")
bash refresh-codex-auth.sh default
jq -e '.tokens.access_token == "fixture-day-two-access" and .tokens.refresh_token == "fixture-day-two-refresh"' "$github_login" >/dev/null
kubectl get secret aloop-codex-recovery -o json | jq -j '.data["auth.json"] | @base64d' | cmp - "$github_login"
kubectl exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json | cmp - "$github_login"
rotated=$(cat "$github_login")
CODEX_AUTH_JSON=$rotated
if bash refresh-codex-auth.sh default; then exit 1; fi
test "$(kubectl get secret aloop-codex-recovery -o json | jq -r '.data["auth.json"] | @base64d')" = "$rotated"
test "$(cat "$github_login")" = "$rotated"

# Redeploying the mirrored secret must preserve a newer login on the volume.
newer=$(jq -c '.tokens.access_token = "renewed-by-worker"' <<< "$rotated")
printf '%s' "$newer" | kubectl exec -i deployment/aloop -- sh -ec 'cat > "$CODEX_HOME/auth.json"'
bash prepare-credentials.sh default
kubectl apply -k .
kubectl rollout status deployment/aloop --timeout=3m
kubectl exec deployment/aloop -- test -f Maildir/tmp/persistence-test
test "$(kubectl exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json)" = "$newer"
kubectl exec deployment/aloop -- sh -ec 'rm "$CODEX_HOME/auth.json"'
kubectl rollout restart deployment/aloop
kubectl rollout status deployment/aloop --timeout=3m
test "$(kubectl exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json)" = "$rotated"
export CODEX_AUTH_JSON='{"tokens":{"access_token":"replacement-fixture"}}'
bash prepare-credentials.sh default
kubectl apply -k .
kubectl rollout status deployment/aloop --timeout=3m
test "$(kubectl exec deployment/aloop -- cat /var/lib/aloop/codex/auth.json)" = "$CODEX_AUTH_JSON"
