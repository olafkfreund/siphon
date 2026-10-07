# VM test helper: talk to `siphon mcp` like an MCP client (stdin stays open,
# one request at a time) and print the tool names, one per line.
import json
import subprocess
import sys

p = subprocess.Popen(sys.argv[1:], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)


def send(m):
    p.stdin.write(json.dumps(m) + "\n")
    p.stdin.flush()


def reply(i):
    for line in p.stdout:
        m = json.loads(line)
        if m.get("id") == i:
            return m
    sys.exit("siphon mcp exited before replying")


send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "vm", "version": "1"}}})
reply(1)
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
for t in reply(2)["result"]["tools"]:
    print(t["name"])
p.kill()
