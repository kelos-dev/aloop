#!/usr/bin/env bash
set -euo pipefail

namespace=${1:?supply the dev namespace}
: "${GITHUB_REPOSITORY:?missing GitHub repository}"
umask 077
directory=$(mktemp -d)
trap 'rm -rf "$directory"' EXIT
k=(kubectl -n "$namespace")
github_secret_version() {
  gh secret list --repo "$GITHUB_REPOSITORY" --env dev --json name,updatedAt \
    --jq '.[] | select(.name == "CODEX_AUTH_JSON") | .updatedAt'
}
github_version=$(github_secret_version)
pod=$("${k[@]}" get pods -l app.kubernetes.io/name=aloop --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}')
: "${pod:?aloop must be running}"
"${k[@]}" wait --for=condition=Ready "pod/$pod" --timeout=1m
"${k[@]}" exec "$pod" -c aloop -- sh -ec 'cat "$CODEX_HOME/.seed-digest"' > "$directory/seed-digest"
origin=$(cat "$directory/seed-digest")
mounted=$("${k[@]}" exec "$pod" -c aloop -- sha256sum /var/run/aloop-login/auth.json | cut -d ' ' -f 1)
github_digest=$(printf '%s' "${CODEX_AUTH_JSON:-}" | sha256sum | cut -d ' ' -f 1)

# Only a backup from this login can identify a previously published GitHub secret.
printf '%s\n' "$origin" > "$directory/known"
"${k[@]}" get secret aloop-codex-recovery --ignore-not-found -o json > "$directory/recovery"
if [[ -s $directory/recovery && $(jq -r '.data["seed-digest"] | @base64d' "$directory/recovery") == "$origin" ]]; then
  jq -r '.data["login-digests"] | @base64d' "$directory/recovery" >> "$directory/known"
  jq -j '.data["auth.json"] | @base64d' "$directory/recovery" > "$directory/auth.json"
fi
if ! grep -Fxq "$mounted" "$directory/known" ||
   { [[ -n ${CODEX_AUTH_JSON:-} ]] && ! grep -Fxq "$github_digest" "$directory/known"; }; then
  echo 'Deploy the replacement Codex login before running refresh.' >&2
  exit 1
fi

publish() {
  local current
  current=$(github_secret_version)
  if [[ $current != "$github_version" ]]; then
    echo 'The GitHub login changed during refresh. Deploy it before retrying.' >&2
    return 1
  fi
  gh secret set CODEX_AUTH_JSON --repo "$GITHUB_REPOSITORY" --env dev < "$directory/auth.json"
}

# Resume a failed GitHub write from the saved bundle without spending another refresh token.
if [[ -s $directory/auth.json && $(sha256sum "$directory/auth.json" | cut -d ' ' -f 1) != "$github_digest" ]]; then
  publish
  exit
fi

save_backup() {
  # kubectl can include Secret data in an API error.
  if ! "${k[@]}" create secret generic aloop-codex-recovery --dry-run=client -o json \
    --from-file="$directory/auth.json" --from-file="$directory/seed-digest" --from-file="$directory/login-digests" |
    "${k[@]}" apply --server-side --field-manager=aloop-refresh "$@" -f - 2>/dev/null; then
    echo "Cannot apply the aloop-codex-recovery Secret in namespace $namespace" >&2
    return 1
  fi
}

# The mount and the GitHub secret can advance on different days between deployments.
printf '%s\n' "$mounted" "$github_digest" > "$directory/login-digests"
touch "$directory/auth.json"
save_backup --dry-run=server
"${k[@]}" exec -i "$pod" -c aloop -- sh -es > "$directory/auth.json" <<'POD'
exec 9>"$CODEX_HOME/.cli.lock"
flock --wait 180 9
aloop -refresh-codex-login
cat "$CODEX_HOME/auth.json"
POD
sha256sum "$directory/auth.json" | cut -d ' ' -f 1 >> "$directory/login-digests"
save_backup
publish
