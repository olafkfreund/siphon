#!/usr/bin/env bash
# CI check for the microVM (plan #14 step 4): boot it with a fresh directory,
# run a webhook-triggered job, restart, and prove the state volume kept it.
set -euo pipefail
export SIPHON_VM_DIR=$(mktemp -d)
log=$SIPHON_VM_DIR.log
runner=$(nix build --print-out-paths .#microvm)/bin/siphon-microvm
cleanup() { pkill -f "microvm@siphon-vm" || true; rm -rf "$SIPHON_VM_DIR"; }
trap cleanup EXIT

boot() {
  "$runner" >"$log" 2>&1 </dev/null &
  for _ in $(seq 150); do curl -sf -m 2 http://127.0.0.1:8090/healthz >/dev/null && return 0; sleep 2; done
  tail -50 "$log"; return 1
}
jobs_api() { curl -sf -H "Authorization: Bearer $(cat "$SIPHON_VM_DIR/config/token")" http://127.0.0.1:8090/api/jobs; }

boot
body='{"kind":"echo","msg":"ci"}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$(cat "$SIPHON_VM_DIR/config/hook")" | awk '{print $2}')
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Hub-Signature-256: sha256=$sig" -d "$body" http://127.0.0.1:8090/hook/test)
[ "$code" = 202 ] || { echo "webhook: $code"; exit 1; }
for _ in $(seq 30); do jobs_api | grep -q '"state":"done"' && break; sleep 1; done
jobs_api | grep -q '"state":"done"' || { jobs_api; exit 1; }

pkill -f "microvm@siphon-vm"; sleep 5
# A run must not read the shared config's secrets (the portal token).
cat >>"$SIPHON_VM_DIR/config/siphon.yaml" <<'YAML'
  - { name: peek, source: test, when: 'event.kind == "peek"', on: each, id: event.msg, action: { cmd: [cat, /etc/siphon/token] } }
YAML
boot
jobs_api | grep -q '"rule":"echo"' || { echo "job lost across restart"; jobs_api; exit 1; }
body='{"kind":"peek","msg":"ci"}'
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$(cat "$SIPHON_VM_DIR/config/hook")" | awk '{print $2}')
curl -sf -o /dev/null -X POST -H "X-Hub-Signature-256: sha256=$sig" -d "$body" http://127.0.0.1:8090/hook/test
for _ in $(seq 30); do jobs_api | grep -Eq '"rule":"peek"[^}]*"state":"(done|failed)"' && break; sleep 1; done
jobs_api | grep -Eq '"rule":"peek"[^}]*"state":"failed"' || { echo "a run read /etc/siphon/token"; jobs_api; exit 1; }
echo "microvm check passed"
