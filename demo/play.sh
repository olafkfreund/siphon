#!/usr/bin/env bash
# The demo, "a bad deploy, start to finish": three acts against the demo VM.
#   demo-play          pauses for Enter between the acts; you approve in the portal
#   demo-play --auto   no pauses, and the CLI approves (for the VM test)
# Env: SIPHON_URL (http://127.0.0.1:8091), SIPHON_DEMO_DIR (~/.config/siphon-demo),
#      DEMO_SHARE (the demo/ directory; the Nix package sets it).
# No secret is ever put on argv or printed: keys are read by python from their
# files, and curl gets its headers from a 0600 file in a temp dir.
set -euo pipefail

auto=0
[ "${1:-}" = "--auto" ] && auto=1

dir=${SIPHON_DEMO_DIR:-$HOME/.config/siphon-demo}
share=${DEMO_SHARE:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}
export SIPHON_URL=${SIPHON_URL:-http://127.0.0.1:8091}
export SIPHON_TOKEN_FILE=${SIPHON_TOKEN_FILE:-$dir/config/token}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say() { printf '\n== %s\n' "$*"; }
pause() { [ "$auto" = 1 ] || read -r -p "-- press Enter for the next act " _; }
die() { printf 'demo-play: %s\n' "$*" >&2; exit 1; }

# apply_task <name> <sed args...>: template | sed, check the edit, dry-run, apply.
apply_task() {
  local name=$1 expect=$2
  shift 2
  siphon template "$name" | sed "$@" >"$tmp/task.yaml"
  grep -q -- "$expect" "$tmp/task.yaml" || die "template $name: the edit '$expect' did not apply"
  apply_file "$tmp/task.yaml"
}

apply_file() {
  local dry
  dry=$(siphon apply -f "$1" --dry-run -o json) || true
  printf '%s' "$dry" | python3 -I -c 'import json,sys; sys.exit(1 if json.load(sys.stdin).get("errors") else 0)' ||
    die "dry-run of $1 has errors: $dry"
  siphon apply -f "$1" --yes >/dev/null
}

# send <source> <payload|-> <kind>: sign and POST one webhook. kind: github | kuma | morning.
send() {
  local source=$1 payload=$2 kind=$3
  python3 -I - "$kind" "$payload" "$dir/config" "$tmp" <<'PY'
import hashlib, hmac, json, os, sys, time
kind, payload, cfg, out = sys.argv[1:5]
os.umask(0o077)
def key(name):
    return open(os.path.join(cfg, name), "rb").read().strip()
if kind == "morning":
    body = json.dumps({"day": time.strftime("%F-%T")}).encode()
else:
    d = json.load(open(payload))
    if kind == "github":
        d["workflow_run"]["id"] = int(time.time())
    else:
        d["heartbeat"]["time"] = time.strftime("%F %T") + ".000"
    body = json.dumps(d).encode()
if kind == "github":
    sig = "sha256=" + hmac.new(key("hook"), body, hashlib.sha256).hexdigest()
    hdr = "X-Hub-Signature-256: %s\nX-GitHub-Event: workflow_run\n" % sig
else:
    hdr = "X-Siphon-Key: %s\n" % key("kuma").decode()
hdr += "Content-Type: application/json\n"
open(os.path.join(out, "body"), "wb").write(body)
open(os.path.join(out, "hdr"), "w").write(hdr)
PY
  curl -sf --max-time 10 -H "@$tmp/hdr" --data-binary "@$tmp/body" "$SIPHON_URL/hook/$source" >/dev/null ||
    die "posting to /hook/$source failed"
  rm -f "$tmp/hdr" "$tmp/body"
}

# last_id: the newest job id, so an act only looks at jobs created after it.
last_id() {
  siphon jobs ls -o json | python3 -I -c 'import json,sys; print(max([j["id"] for j in json.load(sys.stdin)] or [0]))'
}

# wait_job <rule> <state> <timeout s> <act> <after id>: print the id of the newest job
# of that rule, past <after id>, once it is in <state>. A failed job or a timeout is fatal.
wait_job() {
  local rule=$1 state=$2 timeout=$3 act=$4 after=$5 end res
  end=$((SECONDS + timeout))
  while :; do
    res=$(siphon jobs ls -o json | python3 -I -c '
import json, sys
rule, state, after = sys.argv[1], sys.argv[2], int(sys.argv[3])
js = [j for j in json.load(sys.stdin) if j["rule"] == rule and j["id"] > after]
js.sort(key=lambda j: -j["id"])
if js and js[0]["state"] == state:
    print(js[0]["id"])
elif js and js[0]["state"] in ("failed", "denied"):
    print("failed")
' "$rule" "$state" "$after")
    case $res in
    "") ;;
    failed) die "$act: the $rule job failed (siphon jobs show)" ;;
    *) echo "$res"; return 0 ;;
    esac
    [ "$SECONDS" -lt "$end" ] || die "$act: timed out after ${timeout}s waiting for $rule to be $state"
    sleep 2
  done
}

# show_output <id> [step]: the job's output; for a routine, one step's output.
show_output() {
  siphon jobs show "$1" -o json | python3 -I -c '
import json, sys
out = json.load(sys.stdin).get("output", "")
if len(sys.argv) > 2:
    out = json.loads(out)["steps"][sys.argv[2]]["output"]
print(out)
' "$@"
}

say "Setup: applying the three tasks"
apply_task github-ci-failure "kind: model" \
  -e 's/kind: claude/kind: model/' -e 's/credential: claude-max/credential: ollama-local\n    model: llama3.2/'
apply_task uptime-kuma-down "18099/kuma" -e 's#https://ntfy.sh/siphon-CHANGE-ME#http://127.0.0.1:18099/kuma#'
apply_task weekly-digest "18099/digest" \
  -e 's#https://metrics.example.com/api/last-week#http://127.0.0.1:18099/#' \
  -e 's#https://ntfy.sh/siphon-CHANGE-ME#http://127.0.0.1:18099/digest#'
apply_file "$share/tasks/morning.yaml"

say "Act 1: a CI run fails on main (GitHub workflow_run webhook)"
base=$(last_id)
send github-hooks "$share/payloads/workflow-run-failed.json" github
id=$(wait_job ci-failed pending_approval 120 "act 1" "$base")
echo "Job $id is waiting for approval; the phone got the notification."
if [ "$auto" = 1 ]; then
  siphon approve "$id"
else
  echo "Approve it in the portal ($SIPHON_URL): Approvals, then Approve."
fi
id=$(wait_job ci-failed "done" 900 "act 1" "$base") # interactive: time to talk before approving
say "Diagnosis (job $id)"
show_output "$id"
pause

say "Act 2: the site goes down (Uptime Kuma webhook)"
base=$(last_id)
send uptime-kuma "$share/payloads/kuma-down.json" kuma
id=$(wait_job monitor-down "done" 120 "act 2" "$base")
echo "Job $id sent the plain notification."
pause

say "Act 3: the morning digest (a routine)"
base=$(last_id)
send morning - morning
id=$(wait_job morning-digest "done" 300 "act 3" "$base")
say "Digest (job $id)"
show_output "$id" digest

say "That's the story: three sources, three rules, one approval, three jobs."
echo "Look around: $SIPHON_URL/jobs, and the notification log at http://127.0.0.1:8092"
