#!/usr/bin/env bash
# CI check for the OCI image (plan #14 step 3): run it the documented way
# (rootless podman, no capabilities, read-only root) and prove it serves,
# runs a webhook-triggered job, refuses sandbox: systemd, and is non-root.
set -euo pipefail
dir=$(mktemp -d)
trap 'podman rm -f siphon-ci >/dev/null 2>&1 || true; podman volume rm -f siphon-ci-state >/dev/null 2>&1 || true; rm -rf "$dir"' EXIT

"$(nix build --print-out-paths .#image)" | podman load
img=ghcr.io/olafkfreund/siphon:$(nix eval --raw .#siphon.version)
bin=$(podman image inspect "$img" --format '{{index .Config.Entrypoint 0}}')

printf 'test-token-0123456789abcdef0123456789' >"$dir/token"
printf 'hook-secret' >"$dir/hook"
cat >"$dir/siphon.yaml" <<YAML
server:
  db: /var/lib/siphon/state.db
  token: file:/etc/siphon/token
  # no sandbox: inside a container it must default to none
sources:
  gh: { type: webhook, secret: file:/etc/siphon/hook, signature: github }
rules:
  - { name: ver, source: gh, when: 'event.kind == "ver"', on: each, id: event.n, action: { cmd: ["$bin", "version"] } }
YAML
chmod 700 "$dir"; chmod 600 "$dir"/*   # private; keep-id maps us to uid 65532 inside

podman run -d --name siphon-ci --cap-drop=all --read-only --tmpfs /tmp \
  --userns=keep-id:uid=65532,gid=65532 \
  -p 127.0.0.1:18095:8080 -v "$dir":/etc/siphon:ro -v siphon-ci-state:/var/lib/siphon "$img" >/dev/null
for _ in $(seq 60); do curl -sf http://127.0.0.1:18095/healthz >/dev/null && break; sleep 0.5; done
curl -sf http://127.0.0.1:18095/healthz

body='{"kind":"ver","n":1}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac hook-secret | awk '{print $2}')
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Hub-Signature-256: sha256=$sig" -d "$body" http://127.0.0.1:18095/hook/gh)
[ "$code" = 202 ] || { echo "webhook: $code"; exit 1; }
for _ in $(seq 30); do
  podman exec siphon-ci "$bin" jobs ls -config /etc/siphon/siphon.yaml 2>/dev/null | grep -Eq '^1 +ver +done' && break
  sleep 1
done
podman exec siphon-ci "$bin" jobs ls -config /etc/siphon/siphon.yaml 2>/dev/null | grep -Eq '^1 +ver +done' || { podman logs siphon-ci; exit 1; }
podman logs siphon-ci 2>&1 | grep -q 'runs are not isolated' || { echo "no unsandboxed warning"; exit 1; }

# sandbox: systemd inside a container is an error, not a half-working sandbox
sed 's/  # no sandbox.*/  sandbox: systemd/' "$dir/siphon.yaml" >"$dir/systemd.yaml"; chmod 600 "$dir/systemd.yaml"
if out=$(podman run --rm --userns=keep-id:uid=65532,gid=65532 -v "$dir":/etc/siphon:ro "$img" validate -config /etc/siphon/systemd.yaml 2>&1); then
  echo "sandbox: systemd was accepted in a container"; exit 1
fi
grep -q 'systemd sandbox is not available inside a container' <<<"$out" || { echo "$out"; exit 1; }

[ "$(podman image inspect "$img" --format '{{.Config.User}}')" = 65532:65532 ] || { echo "image not non-root"; exit 1; }
echo "container check passed"
