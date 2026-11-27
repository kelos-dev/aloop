#!/usr/bin/env bash
set -euo pipefail

: "${DEV_CLUSTER_KUBECONFIG:?missing DEV_CLUSTER_KUBECONFIG}"
: "${TUNNEL_SERVICE_TOKEN_ID:?missing CLOUDFLARE_ACCESS_CLIENT_ID}"
: "${TUNNEL_SERVICE_TOKEN_SECRET:?missing CLOUDFLARE_ACCESS_CLIENT_SECRET}"

version=2026.10.5
checksum=149d1ec4ac8e58bdbf08a41cc42347da9184d9fb4ef0df6528d75c90a85ac0ed
umask 077
directory=$(mktemp -d)
proxy=
cleanup() {
  if [[ -n $proxy ]]; then kill "$proxy" 2>/dev/null || true; fi
  rm -rf "$directory"
}
trap cleanup EXIT
curl -fsSL --retry 3 -o "$directory/cloudflared.deb" \
  "https://github.com/cloudflare/cloudflared/releases/download/$version/cloudflared-linux-amd64.deb"
echo "$checksum  $directory/cloudflared.deb" | sha256sum --check
sudo dpkg --install "$directory/cloudflared.deb"

export KUBECONFIG="$directory/kubeconfig"
printf '%s' "$DEV_CLUSTER_KUBECONFIG" > "$KUBECONFIG"
cluster=$(kubectl config view --minify -o jsonpath='{.contexts[0].context.cluster}')
kubectl config set-cluster "$cluster" --server=https://127.0.0.1:6443
cloudflared access tcp --hostname dev-cluster.kelos.dev --url 127.0.0.1:6443 &
proxy=$!
for _ in {1..30}; do
  if nc -z 127.0.0.1 6443; then break; fi
  kill -0 "$proxy"
  sleep 1
done
kubectl --request-timeout=15s get --raw=/readyz
"$@"
